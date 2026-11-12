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
	t := tableOpts{Group: q.Get("group"), Q: strings.TrimSpace(q.Get("q")), Sort: q.Get("sort"), Dir: q.Get("dir"),
		Page: parsePage(q.Get("page")), Per: parsePer(q.Get("per"), defaultPerPage)}
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
	q := cloneValues(base)
	if t.Group != "" {
		q.Set("group", t.Group)
	}
	if t.Q != "" {
		q.Set("q", t.Q)
	}
	q.Set("sort", t.Sort)
	q.Set("dir", t.Dir)
	if t.Per != defaultPerPage {
		q.Set("per", perValue(t.Per))
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	return q
}

func cloneValues(v url.Values) url.Values {
	q := url.Values{}
	for k, x := range v {
		q[k] = append([]string(nil), x...)
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

type tableView struct {
	Opts      tableOpts
	Groups    []string
	PerOpts   []perOption
	Heads     []tableHead
	Rows      []dashRow
	Pager     pager
	SortField string // hidden inputs that keep the order when the controls change
	DirField  string
	Keep      url.Values // other settings of the page (the matrix) the controls must keep
}

func (s *Server) buildTable(d *dash.Dashboard, base url.Values, t tableOpts, row func(*dash.StudentStat) dashRow) tableView {
	v := tableView{Opts: t, Keep: base}
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
	v.PerOpts = perChoices(t.Per, perPageOptions, s.t("dash.per_all"))

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
	pg, lo, hi := paginate(len(rows), t.Page, t.Per, func(p int) string { return "/admin/dashboard?" + t.query(base, p).Encode() })
	v.Pager, v.Opts.Page = pg, pg.Page
	for _, st := range rows[lo:hi] {
		v.Rows = append(v.Rows, row(st))
	}
	v.SortField, v.DirField = t.Sort, t.Dir
	return v
}
