package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"attendance/internal/core"
	"attendance/internal/dash"
	"attendance/internal/hours"
	"attendance/internal/platform"
	"attendance/internal/store"
)

// The Students, Anomalies and Journal tabs of the admin UI. Actions (import,
// codes, corrections…) carry a "return" field that brings the browser back
// to the page they were sent from.

// ---------- Students ----------

const defaultStudentsPer = 30

var studentsPerOptions = []int{10, 30, 50, 100}

// studentSorts: sortable columns of the students list, ascending, and the
// first-click direction.
var studentSorts = map[string]func(a, b *studentRow) int{
	"name":  func(a, b *studentRow) int { return compareNames(a.Name, b.Name) },
	"group": func(a, b *studentRow) int { return compareNames(a.Group, b.Group) },
	"login": func(a, b *studentRow) int { // students without a login last
		if (a.Login == "") != (b.Login == "") {
			return cmp.Compare(boolInt(a.Login == ""), boolInt(b.Login == ""))
		}
		return cmp.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login))
	},
	"status": func(a, b *studentRow) int { // linked, then code sent, then code expired, then not linked
		return cmp.Compare(linkRank(a), linkRank(b))
	},
}

func linkRank(r *studentRow) int {
	switch {
	case r.Linked:
		return 0
	case r.CodeState == "pending":
		return 1
	case r.CodeState == "expired":
		return 2
	}
	return 3
}

type nStudentsData struct {
	studentsOpts
	Rows                              []studentRow // this page
	Groups                            []string
	Total, Linked, Pending, NotLinked int // the group (or archive), before the search and the Telegram filter
	Heads                             []tableHead
	Pager                             pager
	PerOpts                           []perOption
	CSV                               string // export link: the current filters and order, every row
	Return                            string // this page, for the actions' "return" field
}

// studentsOpts are the filters and order of the students list. The CSV
// export takes the same ones.
type studentsOpts struct {
	Group     string
	Archived  bool
	Q         string // part of a name or a login
	TG        string // "linked", "unlinked" (no Telegram account yet) or "" (everyone)
	Sort, Dir string
}

func parseStudentsOpts(q url.Values) studentsOpts {
	o := studentsOpts{Group: q.Get("group"), Archived: q.Get("archived") == "1", Q: strings.TrimSpace(q.Get("q")),
		TG: q.Get("tg"), Sort: q.Get("sort"), Dir: q.Get("dir")}
	if r := []rune(o.Q); len(r) > 100 {
		o.Q = string(r[:100])
	}
	if o.TG != "linked" && o.TG != "unlinked" {
		o.TG = ""
	}
	if _, ok := studentSorts[o.Sort]; !ok {
		o.Sort = "group" // default as before: group, then name
	}
	if o.Dir != "desc" {
		o.Dir = "asc"
	}
	return o
}

// query: the filters (not the order) as URL parameters, defaults left out.
func (o studentsOpts) query() url.Values {
	v := url.Values{}
	if o.Group != "" {
		v.Set("group", o.Group)
	}
	if o.Archived {
		v.Set("archived", "1")
	}
	if o.Q != "" {
		v.Set("q", o.Q)
	}
	if o.TG != "" {
		v.Set("tg", o.TG)
	}
	return v
}

// nameMatches reports whether every word of the search is part of the
// student's name or login. Names are compared as core.NameWords folds them
// (case, ё/е, Kazakh letters, Latin look-alikes), so "нурлан" finds "Нұрлан".
func nameMatches(st *studentRow, search string) bool {
	if search == "" {
		return true
	}
	name := strings.Join(core.NameWords(st.Name), " ")
	login := strings.ToLower(st.Login)
	for _, w := range core.NameWords(search) {
		if !strings.Contains(name, w) && !strings.Contains(login, w) {
			return false
		}
	}
	return true
}

// studentList loads the students of the group and archive filter (all is
// what the summary counts) and returns those that also match the search and
// the Telegram filter, in order.
func (s *Server) studentList(ctx context.Context, o studentsOpts) (all []studentRow, shown []*studentRow, err error) {
	where, args := "s.archived = ?", []any{o.Archived}
	if o.Group != "" {
		where += " AND s.grp = ?"
		args = append(args, o.Group)
	}
	if all, err = s.studentRows(ctx, where, args...); err != nil {
		return nil, nil, err
	}
	for i := range all {
		sr := &all[i]
		if o.TG == "linked" && !sr.Linked || o.TG == "unlinked" && sr.Linked || !nameMatches(sr, o.Q) {
			continue
		}
		shown = append(shown, sr)
	}
	by := studentSorts[o.Sort]
	slices.SortStableFunc(shown, func(a, b *studentRow) int {
		c := by(a, b)
		if o.Dir == "desc" {
			c = -c
		}
		if c == 0 {
			if c = compareNames(a.Group, b.Group); c == 0 {
				c = compareNames(a.Name, b.Name)
			}
		}
		return c
	})
	return all, shown, nil
}

func (s *Server) handleStudents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	o := parseStudentsOpts(q)
	d := &nStudentsData{studentsOpts: o, Return: r.URL.RequestURI()}
	all, rows, err := s.studentList(ctx, o)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if d.Groups, err = s.groups(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	for _, sr := range all {
		d.Total++
		switch {
		case sr.Linked:
			d.Linked++
		case sr.CodeState == "pending":
			d.Pending++
		default:
			d.NotLinked++
		}
	}

	per := parsePer(q.Get("per"), defaultStudentsPer)
	link := func(sortKey, dir string, page int) string {
		v := o.query()
		v.Set("sort", sortKey)
		v.Set("dir", dir)
		if per != defaultStudentsPer {
			v.Set("per", perValue(per))
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		return "/admin/students?" + v.Encode()
	}
	pg, lo, hi := paginate(len(rows), parsePage(q.Get("page")), per, func(p int) string { return link(o.Sort, o.Dir, p) })
	d.Pager = pg
	for _, sr := range rows[lo:hi] {
		d.Rows = append(d.Rows, *sr)
	}
	d.PerOpts = perChoices(per, studentsPerOptions, s.t("dash.per_all"))
	for _, h := range []struct{ key, label string }{{"name", "c.name"}, {"group", "c.group"}, {"login", "plat.col_login"}, {"status", "students.status"}} {
		head := tableHead{Key: h.key, Label: s.t(h.label), URL: link(h.key, "asc", 1)}
		if o.Sort == h.key {
			head.Sort, head.URL = "ascending", link(h.key, "desc", 1)
			if o.Dir == "desc" {
				head.Sort, head.URL = "descending", link(h.key, "asc", 1)
			}
		}
		d.Heads = append(d.Heads, head)
	}
	csvq := o.query()
	csvq.Set("sort", o.Sort)
	csvq.Set("dir", o.Dir)
	d.CSV = "/admin/students.csv?" + csvq.Encode()
	s.render(w, r, http.StatusOK, "students", d)
}

// handleStudentsCSV exports the students list as it is filtered and sorted
// on the page, every row: name, group, platform login and Telegram. The
// Telegram column has the @username, or for an account without a username
// its display name; it is empty for a student who has not linked Telegram.
func (s *Server) handleStudentsCSV(w http.ResponseWriter, r *http.Request) {
	_, rows, err := s.studentList(r.Context(), parseStudentsOpts(r.URL.Query()))
	if err != nil {
		s.serverError(w, err)
		return
	}
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf") // BOM so Excel opens UTF-8 (Cyrillic names) correctly
	cw := csv.NewWriter(&buf)
	cw.Write([]string{"name", "group", "login", "telegram"})
	for _, sr := range rows {
		tg := ""
		switch {
		case sr.Linked && sr.TGUsername != "":
			tg = "@" + sr.TGUsername
		case sr.Linked:
			tg = sr.TGName
		}
		cw.Write([]string{sr.Name, sr.Group, sr.Login, tg})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.serverError(w, err)
		return
	}
	name := "students-" + s.Now().In(s.Cfg.Timezone).Format("2006-01-02") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

type nStudentData struct {
	*studentData
	Return      string
	Points      []core.PointsEntry
	PointsTotal int
	MaxPoints   int
	Platform    *platformUser // the platform user with the student's login
	PlatEvents  []platformUserEvent
	PlatReady   bool      // a sync can run (address and token)
	PlatSynced  time.Time // the last good sync; zero: never
	HelpTotal   int
	HelpWeeks   []helpWeek // weeks the student was named, newest first
}

type helpWeek struct {
	Week string
	N    int
}

func (s *Server) handleStudent(w http.ResponseWriter, r *http.Request) {
	d, err := s.studentPage(r)
	switch {
	case errors.Is(err, errNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.serverError(w, err)
	default:
		nd := nStudentData{studentData: d, Return: r.URL.RequestURI(), MaxPoints: core.MaxPoints}
		if nd.Points, nd.PointsTotal, err = core.StudentPoints(r.Context(), s.DB, d.Student.ID); err != nil {
			s.serverError(w, err)
			return
		}
		if nd.Platform, err = s.studentPlatform(r.Context(), d.Student.Login); err != nil {
			s.serverError(w, err)
			return
		}
		if nd.Platform != nil {
			if nd.PlatEvents, err = s.userEvents(r.Context(), nd.Platform); err != nil {
				s.serverError(w, err)
				return
			}
		}
		nd.PlatReady = s.syncer().Ready()
		if st, err := platform.LoadStatus(r.Context(), s.DB); err == nil {
			nd.PlatSynced = st.LastSync
		}
		if d.Student.Login != "" {
			_, tallies, err := s.helpCounts(r.Context())
			if err != nil {
				s.serverError(w, err)
				return
			}
			for _, t := range tallies {
				for login, n := range t.Counts {
					if strings.EqualFold(login, d.Student.Login) {
						nd.HelpTotal += n
						nd.HelpWeeks = append(nd.HelpWeeks, helpWeek{t.Week.Week, n})
					}
				}
			}
		}
		s.render(w, r, http.StatusOK, "student", nd)
	}
}

func (s *Server) handleSlips(w http.ResponseWriter, r *http.Request) {
	slips, err := s.slips(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "slips", slips)
}

// ---------- Anomalies ----------

type anomVisit struct {
	Student    core.Student
	Host       string
	Scan       time.Time // check-in
	Start, End time.Time
	Open       bool
	Duration   time.Duration
	Gap        time.Duration // median gap between active moments
	Active     int           // active pings
	Low        bool
	Long       bool
	Old        bool  // scanned an old QR code (dash.IsOldScan)
	LatencyMs  int64 // age of the code when it was scanned
	Stale      bool  // the screen was already showing a newer code
	Flags      []string
}

type nAnomData struct {
	From, To       string
	Gap, LongVisit time.Duration
	Latency        time.Duration
	Low, Long, Old []anomVisit
	Multi          []anomVisit // visits with two or three flags
	Scans          *scanAnoms  // several PCs, several students, auto-closed, failed scans
}

func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	d, err := s.newAnomalies(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "anomalies", d)
}

// newAnomalies flags the visits whose check-in falls in the period (default
// the last 7 days) as low-activity, long or old-code; visits with two or three
// of these are listed again together. Manual visits added by staff have no
// check-in and no pings and are not judged.
func (s *Server) newAnomalies(r *http.Request) (*nAnomData, error) {
	ctx := r.Context()
	from, last := s.dateRange(r.URL.Query(), "from", "to", s.Calc.PeriodAt("week", s.Now())) // default: this week
	to := s.Calc.DayStart(last.AddDate(0, 0, 1).Add(12 * time.Hour))
	c := s.Cfg
	d := &nAnomData{From: from.Format("2006-01-02"), To: to.AddDate(0, 0, -1).Format("2006-01-02"),
		Gap: c.AnomalyLowActivityGap, LongVisit: c.AnomalyLongVisit, Latency: c.AnomalyLatency}

	visits, err := hours.Load(ctx, s.DB, from, to, 0)
	if err != nil {
		return nil, err
	}
	students, err := s.loadStudents(ctx)
	if err != nil {
		return nil, err
	}
	var judged []hours.Visit
	var sessions []int64
	lo, hi := to, from
	for _, v := range visits {
		scan := v.Start
		if v.Edited {
			scan = v.OrigStart
		}
		if v.Deleted || v.Manual || scan.Before(from) || !scan.Before(to) {
			continue
		}
		judged = append(judged, v)
		sessions = append(sessions, v.SessionID)
		lo, hi = minT(lo, v.Start), maxT(hi, v.End)
	}
	pings, err := s.sessionPings(ctx, sessions, lo, hi)
	if err != nil {
		return nil, err
	}
	for _, v := range judged {
		a := anomVisit{Student: students[v.StudentID], Host: v.Host, Scan: v.Start, Start: v.Start, End: v.End,
			Open: v.Open, Duration: v.Duration(), LatencyMs: v.LatencyMs, Stale: v.Stale}
		if v.Edited {
			a.Scan = v.OrigStart
		}
		a.Student.ID = v.StudentID
		gap, active, ok := dash.ActivityGap(v.Start, v.End, pings[v.SessionID])
		a.Gap, a.Active = gap, active
		a.Low = ok && gap >= c.AnomalyLowActivityGap
		a.Long = a.Duration > c.AnomalyLongVisit
		a.Old = dash.IsOldScan(v.Stale, time.Duration(v.LatencyMs)*time.Millisecond, c.AnomalyLatency)
		for _, f := range []struct {
			on  bool
			key string
		}{{a.Low, "nanom.f_low"}, {a.Long, "nanom.f_long"}, {a.Old, "nanom.f_old"}} {
			if f.on {
				a.Flags = append(a.Flags, s.t(f.key))
			}
		}
		if a.Low {
			d.Low = append(d.Low, a)
		}
		if a.Long {
			d.Long = append(d.Long, a)
		}
		if a.Old {
			d.Old = append(d.Old, a)
		}
		if len(a.Flags) >= 2 {
			d.Multi = append(d.Multi, a)
		}
	}
	newestFirst := func(x, y anomVisit) int { return y.Scan.Compare(x.Scan) }
	for _, l := range [][]anomVisit{d.Low, d.Long, d.Old, d.Multi} {
		slices.SortStableFunc(l, newestFirst)
	}
	if d.Scans, err = s.scanAnomalies(ctx, from, to); err != nil {
		return nil, err
	}
	return d, nil
}

// sessionPings returns the agent heartbeats (with a known idle time) of the
// given PC sessions between lo and hi, by session id.
func (s *Server) sessionPings(ctx context.Context, ids []int64, lo, hi time.Time) (map[int64][]dash.Ping, error) {
	out := map[int64][]dash.Ping{}
	if len(ids) == 0 {
		return out, nil
	}
	type key struct{ host, sid string }
	bySID := map[key][]int64{}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for len(ids) > 0 {
		n := min(len(ids), 500)
		var list []string
		for _, id := range ids[:n] {
			list = append(list, strconv.FormatInt(id, 10))
		}
		ids = ids[n:]
		rows, err := s.DB.QueryContext(ctx, "SELECT id, host, sid FROM sessions WHERE id IN ("+strings.Join(list, ",")+")")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var k key
			if err := rows.Scan(&id, &k.host, &k.sid); err != nil {
				rows.Close()
				return nil, err
			}
			bySID[k] = append(bySID[k], id)
		}
		rows.Close()
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT host, sid, at, idle_ms FROM events
		WHERE kind = 'heartbeat' AND idle_ms IS NOT NULL AND at >= ? AND at <= ?`, store.Ms(lo), store.Ms(hi))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k key
		var at, idle int64
		if err := rows.Scan(&k.host, &k.sid, &at, &idle); err != nil {
			return nil, err
		}
		// A session id can repeat on a PC only after the earlier session
		// ended; ActivityGap only looks at pings inside each visit.
		for _, id := range bySID[k] {
			out[id] = append(out[id], dash.Ping{At: store.FromMs(at), Idle: time.Duration(idle) * time.Millisecond})
		}
	}
	return out, rows.Err()
}

func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ---------- Journal ----------

const defaultJournalPer = 30

var journalPerOptions = []int{10, 30, 50, 100}

type nJournalData struct {
	Rows    []auditRow
	Pager   pager
	PerOpts []perOption
}

// handleJournal pages the audit log in SQL (newest first).
func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var total int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log").Scan(&total); err != nil {
		s.serverError(w, err)
		return
	}
	per := parsePer(q.Get("per"), defaultJournalPer)
	link := func(p int) string {
		v := url.Values{}
		if per != defaultJournalPer {
			v.Set("per", perValue(per))
		}
		if p > 1 {
			v.Set("page", strconv.Itoa(p))
		}
		if len(v) == 0 {
			return "/admin/journal"
		}
		return "/admin/journal?" + v.Encode()
	}
	pg, lo, hi := paginate(total, parsePage(q.Get("page")), per, link)
	d := nJournalData{Pager: pg, PerOpts: perChoices(per, journalPerOptions, s.t("dash.per_all"))}
	rows, err := s.DB.QueryContext(ctx, "SELECT at, actor, action, target, details FROM audit_log ORDER BY id DESC LIMIT ? OFFSET ?", hi-lo, lo)
	if err != nil {
		s.serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var a auditRow
		var at int64
		if err := rows.Scan(&at, &a.Actor, &a.Action, &a.Target, &a.Details); err != nil {
			s.serverError(w, err)
			return
		}
		a.At = store.FromMs(at)
		d.Rows = append(d.Rows, a)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "journal", d)
}
