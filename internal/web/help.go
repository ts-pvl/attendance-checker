package web

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"attendance/internal/help"
)

// The Peer help tab: the weekly form "who helped you most", imported per
// week, and how often each student is named (internal/help). Everyone on
// staff can look; admins import files and resolve typed logins.

type helpRow struct {
	Login        string
	StudentID    int64
	Student      string
	Group        string
	PlatformName string
	Week, Total  int
}

// reviewRow groups the typed logins of a week that were not plain matches.
type reviewRow struct {
	Typed       string
	Times       int
	How         help.How
	Login       string // what it was resolved to, "" if unresolved
	Suggest     string
	Respondents string
}

type nHelpData struct {
	Weeks       []help.Week
	Week        string // a Monday, or "all"
	Current     *help.WeekTally
	Rows        []helpRow
	Heads       []tableHead
	Pager       pager
	PerOpts     []perOption
	Groups      []string
	Group       string
	Sort, Dir   string
	Self        string
	Review      []reviewRow
	Logins      []string // for the alias inputs
	DefaultWeek string   // for the import form
	Stats       struct{ Counted, Self, Repeat, Superseded, Unresolved int }
}

var helpSorts = map[string]struct {
	cmp  func(a, b *helpRow) int
	desc bool
}{
	"week":  {func(a, b *helpRow) int { return cmp.Compare(a.Week, b.Week) }, true},
	"total": {func(a, b *helpRow) int { return cmp.Compare(a.Total, b.Total) }, true},
	"login": {func(a, b *helpRow) int { return cmp.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login)) }, false},
	"name": {func(a, b *helpRow) int {
		return compareNames(cmp.Or(a.Student, a.PlatformName), cmp.Or(b.Student, b.PlatformName))
	}, false},
}

// helpCounts counts every week; it is used by the tab and the student page.
func (s *Server) helpCounts(ctx context.Context) (*help.Matcher, []help.WeekTally, error) {
	m, err := help.LoadMatcher(ctx, s.DB)
	if err != nil {
		return nil, nil, err
	}
	tallies, err := help.TallyAll(ctx, s.DB, m)
	return m, tallies, err
}

func (s *Server) handleNewHelp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	_, tallies, err := s.helpCounts(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	d := &nHelpData{Week: q.Get("week"), Group: q.Get("group"),
		DefaultWeek: s.Calc.PeriodAt("week", s.Now().AddDate(0, 0, -3)).From.Format("2006-01-02")}
	for _, t := range tallies {
		d.Weeks = append(d.Weeks, t.Week)
	}
	if d.Week != "all" {
		d.Current = nil
		for i := range tallies {
			if tallies[i].Week.Week == d.Week || d.Week == "" && i == 0 {
				d.Current = &tallies[i]
			}
		}
		if d.Current != nil {
			d.Week = d.Current.Week.Week
		} else if len(tallies) > 0 {
			d.Week = "all"
		}
	}

	// Who the logins are.
	type who struct {
		id                  int64
		name, group, pfName string
	}
	people := map[string]*who{}
	rows, err := s.DB.QueryContext(ctx, "SELECT id, name, grp, login FROM students WHERE login <> ''")
	if err != nil {
		s.serverError(w, err)
		return
	}
	for rows.Next() {
		var x who
		var login string
		if err := rows.Scan(&x.id, &x.name, &x.group, &login); err != nil {
			rows.Close()
			s.serverError(w, err)
			return
		}
		people[strings.ToLower(login)] = &x
	}
	rows.Close()
	pus, err := s.platformUsers(ctx, "1 = 1")
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, u := range pus {
		k := strings.ToLower(u.Login)
		if people[k] == nil {
			people[k] = &who{}
		}
		people[k].pfName = u.Name
		d.Logins = append(d.Logins, u.Login)
	}
	for k, x := range people {
		if x.id != 0 && !slices.ContainsFunc(d.Logins, func(l string) bool { return strings.EqualFold(l, k) }) {
			d.Logins = append(d.Logins, k)
		}
	}
	slices.SortFunc(d.Logins, func(a, b string) int { return cmp.Compare(strings.ToLower(a), strings.ToLower(b)) })

	// Counts: this week and all weeks.
	byLogin := map[string]*helpRow{}
	row := func(login string) *helpRow {
		k := strings.ToLower(login)
		if byLogin[k] == nil {
			hr := &helpRow{Login: login}
			if x := people[k]; x != nil {
				hr.StudentID, hr.Student, hr.Group, hr.PlatformName = x.id, x.name, x.group, x.pfName
			}
			byLogin[k] = hr
		}
		return byLogin[k]
	}
	for i := range tallies {
		for login, n := range tallies[i].Counts {
			hr := row(login)
			hr.Total += n
			if d.Current != nil && d.Current.ID == tallies[i].ID {
				hr.Week += n
			}
		}
	}
	groups := map[string]bool{}
	var list []*helpRow
	for _, hr := range byLogin {
		if hr.Group != "" {
			groups[hr.Group] = true
		}
		if d.Group != "" && hr.Group != d.Group || d.Current != nil && hr.Week == 0 {
			continue
		}
		list = append(list, hr)
	}
	for g := range groups {
		d.Groups = append(d.Groups, g)
	}
	slices.Sort(d.Groups)

	defSort := "week"
	if d.Current == nil {
		defSort = "total"
	}
	d.Sort, d.Dir = q.Get("sort"), q.Get("dir")
	key, ok := helpSorts[d.Sort]
	if !ok || d.Current == nil && d.Sort == "week" {
		d.Sort, key = defSort, helpSorts[defSort]
	}
	if d.Dir != "asc" && d.Dir != "desc" {
		d.Dir = map[bool]string{true: "desc", false: "asc"}[key.desc]
	}
	slices.SortStableFunc(list, func(a, b *helpRow) int {
		c := key.cmp(a, b)
		if d.Dir == "desc" {
			c = -c
		}
		if c == 0 {
			if c = cmp.Compare(b.Total, a.Total); c == 0 {
				c = cmp.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login))
			}
		}
		return c
	})
	per := parsePer(q.Get("per"), recentDefaultPer)
	link := func(sortKey, dir string, page int) string {
		v := url.Values{}
		if d.Week != "" {
			v.Set("week", d.Week)
		}
		if d.Group != "" {
			v.Set("group", d.Group)
		}
		if sortKey != defSort || dir != "desc" {
			v.Set("sort", sortKey)
			v.Set("dir", dir)
		}
		if per != recentDefaultPer {
			v.Set("per", perValue(per))
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		return "/admin/new/help?" + v.Encode()
	}
	pg, lo, hi := paginate(len(list), parsePage(q.Get("page")), per, func(p int) string { return link(d.Sort, d.Dir, p) })
	d.Pager = pg
	for _, hr := range list[lo:hi] {
		d.Rows = append(d.Rows, *hr)
	}
	d.Self = link(d.Sort, d.Dir, pg.Page)
	d.PerOpts = perChoices(per, perPageOptions, s.t("dash.per_all"))
	heads := []struct {
		key, label string
		num        bool
	}{{"login", "plat.col_login", false}, {"name", "plat.col_student", false}, {"", "c.group", false}}
	if d.Current != nil {
		heads = append(heads, struct {
			key, label string
			num        bool
		}{"week", "help.col_week", true})
	}
	heads = append(heads, struct {
		key, label string
		num        bool
	}{"total", "help.col_total", true})
	for _, h := range heads {
		head := tableHead{Key: h.key, Label: s.t(h.label), Num: h.num}
		if h.key != "" {
			first := map[bool]string{true: "desc", false: "asc"}[helpSorts[h.key].desc]
			head.URL = link(h.key, first, 1)
			if d.Sort == h.key {
				head.Sort = map[string]string{"asc": "ascending", "desc": "descending"}[d.Dir]
				head.URL = link(h.key, map[string]string{"asc": "desc", "desc": "asc"}[d.Dir], 1)
			}
		}
		d.Heads = append(d.Heads, head)
	}

	// What staff should look at this week.
	if d.Current != nil {
		groupsByTyped := map[string]*reviewRow{}
		var order []string
		for _, x := range d.Current.Mentions {
			switch x.Status {
			case help.Counted:
				d.Stats.Counted++
			case help.Self:
				d.Stats.Self++
			case help.Repeat:
				d.Stats.Repeat++
			case help.Superseded:
				d.Stats.Superseded++
			case help.Unresolved:
				d.Stats.Unresolved++
			}
			if x.How == help.Exact || x.Status == help.Superseded {
				continue
			}
			k := help.Normalize(x.Typed)
			rr := groupsByTyped[k]
			if rr == nil {
				rr = &reviewRow{Typed: strings.TrimSpace(x.Typed), How: x.How, Login: x.Login, Suggest: x.Suggest}
				groupsByTyped[k] = rr
				order = append(order, k)
			}
			rr.Times++
			if x.Respondent != "" && !strings.Contains(rr.Respondents, x.Respondent) {
				if rr.Respondents != "" {
					rr.Respondents += ", "
				}
				rr.Respondents += x.Respondent
			}
		}
		for _, k := range order {
			d.Review = append(d.Review, *groupsByTyped[k])
		}
		slices.SortStableFunc(d.Review, func(a, b reviewRow) int { // unresolved first, then most frequent
			if (a.Login == "") != (b.Login == "") {
				return cmp.Compare(boolInt(a.Login != ""), boolInt(b.Login != ""))
			}
			return cmp.Compare(b.Times, a.Times)
		})
	}
	s.render(w, r, http.StatusOK, "nhelp", d)
}

// parseDay reads a local date (YYYY-MM-DD) as noon of that day.
func (s *Server) parseDay(v string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", v, s.Cfg.Timezone)
	return d.Add(12 * time.Hour), err
}

func (s *Server) helpError(err error) string {
	switch {
	case errors.Is(err, help.ErrNoPeerColumns):
		return s.t("help.no_columns")
	case errors.Is(err, help.ErrTooBig):
		return s.t("help.too_big")
	case errors.Is(err, help.ErrNotWorkbook):
		return s.t("help.not_workbook")
	case errors.Is(err, help.ErrBadWeek):
		return s.t("help.bad_week")
	case errors.Is(err, help.ErrUnknownLogin):
		return s.t("help.unknown_login")
	}
	return s.t("c.error", err.Error())
}

func (s *Server) handleHelpImport(w http.ResponseWriter, r *http.Request) {
	// The CSRF check has already parsed the form; help.Read stops at 10 MB.
	f, hdr, err := r.FormFile("file")
	if err != nil {
		back(w, r, "/admin/new/help", s.t("help.no_file"), true)
		return
	}
	defer f.Close()
	day, err := s.parseDay(r.PostFormValue("week"))
	if err != nil {
		back(w, r, "/admin/new/help", s.helpError(help.ErrBadWeek), true)
		return
	}
	week := s.Calc.PeriodAt("week", day).From.Format("2006-01-02")
	answers, err := help.Read(f, hdr.Filename)
	if err != nil {
		back(w, r, "/admin/new/help", s.helpError(err), true)
		return
	}
	replaced, err := help.Import(r.Context(), s.DB, s.Now(), week, hdr.Filename, answers, staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/new/help", s.helpError(err), true)
		return
	}
	msg := s.t("help.imported", len(answers), week)
	if replaced {
		msg += " " + s.t("help.replaced")
	}
	setFlash(w, msg, false)
	http.Redirect(w, r, "/admin/new/help?week="+week, http.StatusSeeOther)
}

func (s *Server) handleHelpAlias(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := help.LoadMatcher(ctx, s.DB)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := help.SetAlias(ctx, s.DB, m, s.Now(), r.PostFormValue("typed"), r.PostFormValue("login"), staffFrom(r).Username); err != nil {
		back(w, r, "/admin/new/help", s.helpError(err), true)
		return
	}
	back(w, r, "/admin/new/help", s.t("help.alias_saved"), false)
}

func (s *Server) handleHelpDelete(w http.ResponseWriter, r *http.Request) {
	if err := help.Delete(r.Context(), s.DB, s.Now(), r.PostFormValue("week"), staffFrom(r).Username); err != nil {
		back(w, r, "/admin/new/help", s.helpError(err), true)
		return
	}
	setFlash(w, s.t("help.deleted"), false)
	http.Redirect(w, r, "/admin/new/help", http.StatusSeeOther)
}
