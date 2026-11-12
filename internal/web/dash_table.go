package web

import (
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"attendance/internal/dash"
)

// The students table of the dashboard is filtered (group, name), sorted and
// paged on the server. Every state is a plain URL, so links work without
// JavaScript; with it, htmx fetches only the table.

const defaultPerPage = 30

var perPageOptions = []int{10, 30, 50, 100} // plus "all"

type tableOpts struct {
	Group, Q  string
	Sort, Dir string
	Page      int
	Per       int // 0 = all
}

// sortKeys lists the sortable columns: how to compare two rows ascending,
// and whether the first click sorts the biggest first.
var sortKeys = map[string]struct {
	cmp  func(a, b *dash.StudentStat) int
	desc bool
}{
	"name":     {func(a, b *dash.StudentStat) int { return compareNames(a.Name, b.Name) }, false},
	"group":    {func(a, b *dash.StudentStat) int { return compareNames(a.Group, b.Group) }, false},
	"status":   {func(a, b *dash.StudentStat) int { return cmp.Compare(boolInt(!a.Active), boolInt(!b.Active)) }, false}, // active first
	"hours":    {func(a, b *dash.StudentStat) int { return cmp.Compare(a.Total, b.Total) }, true},
	"days":     {func(a, b *dash.StudentStat) int { return cmp.Compare(a.Days, b.Days) }, true},
	"att":      {func(a, b *dash.StudentStat) int { return cmp.Compare(a.Attendance, b.Attendance) }, true},
	"perday":   {func(a, b *dash.StudentStat) int { return cmp.Compare(a.PerDay, b.PerDay) }, true},
	"sessions": {func(a, b *dash.StudentStat) int { return cmp.Compare(a.Sessions, b.Sessions) }, true},
	"arrival": {func(a, b *dash.StudentStat) int { // students without an arrival time go last
		if a.HasArrival != b.HasArrival {
			return cmp.Compare(boolInt(!a.HasArrival), boolInt(!b.HasArrival))
		}
		return cmp.Compare(a.AvgArrival, b.AvgArrival)
	}, false},
	"last": {func(a, b *dash.StudentStat) int { return a.LastVisit.Compare(b.LastVisit) }, true},
}

// lower is the key for sorting and searching names: lower case, and ё as е,
// the way Russian lists are ordered (byte order would put ё after я).
func lower(s string) string { return strings.ReplaceAll(strings.ToLower(s), "ё", "е") }

// compareNames orders names the way a Russian list does: case-insensitive,
// ё as е, and Cyrillic letters before Latin ones (code-point order would put
// every Latin name first).
func compareNames(a, b string) int {
	ra, rb := []rune(lower(a)), []rune(lower(b))
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] == rb[i] {
			continue
		}
		ca, cb := ra[i] >= 0x400 && ra[i] <= 0x4ff, rb[i] >= 0x400 && rb[i] <= 0x4ff
		if ca != cb {
			if ca {
				return -1
			}
			return 1
		}
		return cmp.Compare(ra[i], rb[i])
	}
	return cmp.Compare(len(ra), len(rb))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTableOpts(q url.Values) tableOpts {
	t := tableOpts{Group: q.Get("group"), Q: strings.TrimSpace(q.Get("q")), Sort: q.Get("sort"), Dir: q.Get("dir"), Page: 1, Per: defaultPerPage}
	if r := []rune(t.Q); len(r) > 100 {
		t.Q = string(r[:100])
	}
	key, ok := sortKeys[t.Sort]
	if !ok {
		t.Sort, key = "hours", sortKeys["hours"]
	}
	if t.Dir != "asc" && t.Dir != "desc" {
		t.Dir = "asc"
		if key.desc {
			t.Dir = "desc"
		}
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		t.Page = p
	}
	// The menu offers perPageOptions and "all"; a URL may ask for any size.
	if v := q.Get("per"); v == "all" {
		t.Per = 0
	} else if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1000 {
		t.Per = n
	}
	return t
}

// rows returns the students that match the group and name filter, in order.
func (t tableOpts) rows(all []dash.StudentStat) []*dash.StudentStat {
	q := lower(t.Q)
	var out []*dash.StudentStat
	for i := range all {
		st := &all[i]
		if t.Group != "" && st.Group != t.Group || q != "" && !strings.Contains(lower(st.Name), q) {
			continue
		}
		out = append(out, st)
	}
	c := sortKeys[t.Sort].cmp
	slices.SortStableFunc(out, func(a, b *dash.StudentStat) int {
		r := c(a, b)
		if t.Dir == "desc" {
			r = -r
		}
		if r == 0 { // ties: most hours, then name
			if r = cmp.Compare(b.Total, a.Total); r == 0 {
				r = compareNames(a.Name, b.Name)
			}
		}
		return r
	})
	return out
}

// query returns base plus the table settings that differ from the defaults.
func (t tableOpts) query(base url.Values, page int) url.Values {
	q := url.Values{}
	for k, v := range base {
		q[k] = v
	}
	set := func(k, v, def string) {
		if v != def {
			q.Set(k, v)
		}
	}
	set("group", t.Group, "")
	set("q", t.Q, "")
	q.Set("sort", t.Sort)
	q.Set("dir", t.Dir)
	if t.Per == 0 {
		q.Set("per", "all")
	} else if t.Per != defaultPerPage {
		q.Set("per", strconv.Itoa(t.Per))
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	return q
}

type tableHead struct {
	Key, Label string
	Num        bool
	Colspan    int
	URL        string
	Sort       string // "ascending", "descending" or ""
}

type pageLink struct {
	Num     int
	URL     string
	Current bool
	Gap     bool
}

type perOption struct {
	Value, Label string
	Selected     bool
}

type tableView struct {
	Opts      tableOpts
	Groups    []string
	PerOpts   []perOption
	Heads     []tableHead
	Rows      []dashRow
	Total     int
	First     int // 1-based index of the first row shown
	Last      int
	Pages     []pageLink
	Prev      string
	Next      string
	SortField string // hidden inputs that keep the order when the controls change
	DirField  string
}

func (s *Server) buildTable(d *dash.Dashboard, base url.Values, t tableOpts, row func(*dash.StudentStat) dashRow) tableView {
	v := tableView{Opts: t}
	groups := map[string]bool{}
	for _, st := range d.Students {
		if st.Group != "" {
			groups[st.Group] = true
		}
	}
	if t.Group != "" {
		groups[t.Group] = true // keep a chosen group selectable even when it has no rows
	}
	for g := range groups {
		v.Groups = append(v.Groups, g)
	}
	slices.Sort(v.Groups)
	for _, n := range perPageOptions {
		v.PerOpts = append(v.PerOpts, perOption{strconv.Itoa(n), strconv.Itoa(n), t.Per == n})
	}
	v.PerOpts = append(v.PerOpts, perOption{"all", s.t("dash.per_all"), t.Per == 0})

	for _, h := range []struct {
		key, label string
		num        bool
		span       int
	}{
		{"name", "dash.col_student", false, 1}, {"group", "c.group", false, 1}, {"status", "dash.col_status", false, 1},
		{"hours", "dash.col_hours", false, 2}, {"days", "dash.col_days", true, 1}, {"att", "dash.col_att", true, 1},
		{"perday", "dash.col_per_day", true, 1}, {"sessions", "dash.col_sessions", true, 1},
		{"arrival", "dash.col_arrival", true, 1}, {"last", "dash.col_last", true, 1},
	} {
		next := t
		next.Sort = h.key
		next.Dir = parseTableOpts(url.Values{"sort": {h.key}}).Dir // first click: the column's natural order
		head := tableHead{Key: h.key, Label: s.t(h.label), Num: h.num, Colspan: h.span}
		if t.Sort == h.key {
			head.Sort = map[string]string{"asc": "ascending", "desc": "descending"}[t.Dir]
			next.Dir = map[string]string{"asc": "desc", "desc": "asc"}[t.Dir] // second click flips it
		}
		head.URL = "/admin/dashboard?" + next.query(base, 1).Encode()
		v.Heads = append(v.Heads, head)
	}

	rows := t.rows(d.Students)
	v.Total = len(rows)
	per := t.Per
	if per == 0 {
		per = max(v.Total, 1)
	}
	pages := max((v.Total+per-1)/per, 1)
	page := min(t.Page, pages)
	v.Opts.Page = page
	lo, hi := (page-1)*per, min(page*per, v.Total)
	for _, st := range rows[lo:hi] {
		v.Rows = append(v.Rows, row(st))
	}
	if v.Total > 0 {
		v.First, v.Last = lo+1, hi
	}
	link := func(p int) string { return "/admin/dashboard?" + t.query(base, p).Encode() }
	if pages > 1 {
		for p := 1; p <= pages; p++ {
			// First, last and two either side of the current page; gaps between.
			if p != 1 && p != pages && (p < page-2 || p > page+2) {
				if n := len(v.Pages); n > 0 && !v.Pages[n-1].Gap {
					v.Pages = append(v.Pages, pageLink{Gap: true})
				}
				continue
			}
			v.Pages = append(v.Pages, pageLink{Num: p, URL: link(p), Current: p == page})
		}
		if page > 1 {
			v.Prev = link(page - 1)
		}
		if page < pages {
			v.Next = link(page + 1)
		}
	}
	v.SortField, v.DirField = t.Sort, t.Dir
	return v
}
