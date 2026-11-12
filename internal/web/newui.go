package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"attendance/internal/core"
	"attendance/internal/dash"
	"attendance/internal/hours"
	"attendance/internal/store"
)

// The Students, Anomalies and Journal tabs of the new admin UI. Actions
// (import, codes, corrections…) post to the same endpoints as the old UI,
// with a "return" field that brings the browser back here.

// ---------- Students ----------

const defaultStudentsPer = 30

var studentsPerOptions = []int{10, 30, 50, 100}

// studentSorts: sortable columns of the students list, ascending, and the
// first-click direction.
var studentSorts = map[string]func(a, b *studentRow) int{
	"name":  func(a, b *studentRow) int { return compareNames(a.Name, b.Name) },
	"group": func(a, b *studentRow) int { return compareNames(a.Group, b.Group) },
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
	studentsData
	Sort, Dir string
	Heads     []tableHead
	Pager     pager
	PerOpts   []perOption
	Return    string // this page, for the actions' "return" field
}

func (s *Server) handleNewStudents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &nStudentsData{studentsData: studentsData{Group: q.Get("group"), Archived: q.Get("archived") == "1"}, Return: r.URL.RequestURI()}
	where, args := "s.archived = ?", []any{d.Archived}
	if d.Group != "" {
		where += " AND s.grp = ?"
		args = append(args, d.Group)
	}
	all, err := s.studentRows(ctx, where, args...)
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

	// Sort (default as before: group, then name), then one page.
	d.Sort, d.Dir = q.Get("sort"), q.Get("dir")
	if _, ok := studentSorts[d.Sort]; !ok {
		d.Sort = "group"
	}
	if d.Dir != "desc" {
		d.Dir = "asc"
	}
	rows := make([]*studentRow, len(all))
	for i := range all {
		rows[i] = &all[i]
	}
	by := studentSorts[d.Sort]
	slices.SortStableFunc(rows, func(a, b *studentRow) int {
		c := by(a, b)
		if d.Dir == "desc" {
			c = -c
		}
		if c == 0 {
			if c = compareNames(a.Group, b.Group); c == 0 {
				c = compareNames(a.Name, b.Name)
			}
		}
		return c
	})
	per := parsePer(q.Get("per"), defaultStudentsPer)
	base := url.Values{}
	if d.Group != "" {
		base.Set("group", d.Group)
	}
	if d.Archived {
		base.Set("archived", "1")
	}
	link := func(sortKey, dir string, page int) string {
		v := cloneValues(base)
		v.Set("sort", sortKey)
		v.Set("dir", dir)
		if per != defaultStudentsPer {
			v.Set("per", perValue(per))
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		return "/admin/new/students?" + v.Encode()
	}
	pg, lo, hi := paginate(len(rows), parsePage(q.Get("page")), per, func(p int) string { return link(d.Sort, d.Dir, p) })
	d.Pager = pg
	for _, sr := range rows[lo:hi] {
		d.Rows = append(d.Rows, *sr)
	}
	d.PerOpts = perChoices(per, studentsPerOptions, s.t("dash.per_all"))
	for _, h := range []struct{ key, label string }{{"name", "c.name"}, {"group", "c.group"}, {"status", "students.status"}} {
		head := tableHead{Key: h.key, Label: s.t(h.label), URL: link(h.key, "asc", 1)}
		if d.Sort == h.key {
			head.Sort, head.URL = "ascending", link(h.key, "desc", 1)
			if d.Dir == "desc" {
				head.Sort, head.URL = "descending", link(h.key, "asc", 1)
			}
		}
		d.Heads = append(d.Heads, head)
	}
	s.render(w, r, http.StatusOK, "nstudents", d)
}

type nStudentData struct {
	*studentData
	Return string
}

func (s *Server) handleNewStudent(w http.ResponseWriter, r *http.Request) {
	d, err := s.studentPage(r)
	switch {
	case errors.Is(err, errNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.serverError(w, err)
	default:
		s.render(w, r, http.StatusOK, "nstudent", nStudentData{d, r.URL.RequestURI()})
	}
}

func (s *Server) handleNewSlips(w http.ResponseWriter, r *http.Request) {
	slips, err := s.slips(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "nslips", slips)
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
	Late       bool
	Flags      []string
}

type nAnomData struct {
	From, To         string
	Gap, LongVisit   time.Duration
	LateFrom, LateTo string
	Low, Long, Late  []anomVisit
	Multi            []anomVisit
}

func (s *Server) handleNewAnomalies(w http.ResponseWriter, r *http.Request) {
	d, err := s.newAnomalies(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "nanomalies", d)
}

// newAnomalies flags the visits whose check-in falls in the period (default
// the last 7 days) as low-activity, long or late; visits with two or three
// of these are listed again together. Manual visits added by staff have no
// check-in and no pings and are not judged.
func (s *Server) newAnomalies(r *http.Request) (*nAnomData, error) {
	ctx := r.Context()
	loc := s.Cfg.Timezone
	to := s.Calc.PeriodAt("day", s.Now()).To
	from := to.AddDate(0, 0, -7)
	if t, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), loc); err == nil {
		from = t
	}
	if t, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), loc); err == nil {
		to = t.AddDate(0, 0, 1)
	}
	if to.Before(from) {
		from, to = to.AddDate(0, 0, -1), from.AddDate(0, 0, 1)
	}
	c := s.Cfg
	d := &nAnomData{From: from.Format("2006-01-02"), To: to.AddDate(0, 0, -1).Format("2006-01-02"),
		Gap: c.AnomalyLowActivityGap, LongVisit: c.AnomalyLongVisit,
		LateFrom: fmt.Sprintf("%02d:%02d", c.AnomalyLateFrom.Hour, c.AnomalyLateFrom.Minute),
		LateTo:   fmt.Sprintf("%02d:%02d", c.AnomalyLateTo.Hour, c.AnomalyLateTo.Minute)}

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
			Open: v.Open, Duration: v.Duration()}
		if v.Edited {
			a.Scan = v.OrigStart
		}
		a.Student.ID = v.StudentID
		gap, active, ok := dash.ActivityGap(v.Start, v.End, pings[v.SessionID])
		a.Gap, a.Active = gap, active
		a.Low = ok && gap >= c.AnomalyLowActivityGap
		a.Long = a.Duration > c.AnomalyLongVisit
		a.Late = dash.IsLate(a.Scan, loc, c.AnomalyLateFrom.Offset(), c.AnomalyLateTo.Offset())
		for _, f := range []struct {
			on  bool
			key string
		}{{a.Low, "nanom.f_low"}, {a.Long, "nanom.f_long"}, {a.Late, "nanom.f_late"}} {
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
		if a.Late {
			d.Late = append(d.Late, a)
		}
		if len(a.Flags) >= 2 {
			d.Multi = append(d.Multi, a)
		}
	}
	newestFirst := func(x, y anomVisit) int { return y.Scan.Compare(x.Scan) }
	for _, l := range [][]anomVisit{d.Low, d.Long, d.Late, d.Multi} {
		slices.SortStableFunc(l, newestFirst)
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

// handleNewJournal pages the audit log in SQL (newest first).
func (s *Server) handleNewJournal(w http.ResponseWriter, r *http.Request) {
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
			return "/admin/new/journal"
		}
		return "/admin/new/journal?" + v.Encode()
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
	s.render(w, r, http.StatusOK, "njournal", d)
}
