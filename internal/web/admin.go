package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"attendance/internal/bot"
	"attendance/internal/core"
	"attendance/internal/hours"
	"attendance/internal/i18n"
	"attendance/internal/store"
)

func (s *Server) t(key string, args ...any) string { return i18n.T(s.Cfg.Lang, key, args...) }

// loadStudents returns every student by id.
func (s *Server) loadStudents(ctx context.Context) (map[int64]core.Student, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id, name, grp, archived FROM students")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]core.Student{}
	for rows.Next() {
		var st core.Student
		if err := rows.Scan(&st.ID, &st.Name, &st.Group, &st.Archived); err != nil {
			return nil, err
		}
		out[st.ID] = st
	}
	return out, rows.Err()
}

func (s *Server) groups(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT DISTINCT grp FROM students WHERE grp <> '' AND archived = 0 ORDER BY grp")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---------- Today ----------

type todayStudent struct {
	ID    int64
	Name  string
	Since time.Time
}

type todayHost struct {
	Name      string
	Live      bool
	Stale     bool      // no event for more than two heartbeat periods
	LastEver  time.Time // last event from this PC in any session, shown for PCs without one
	Started   time.Time
	LastSeen  time.Time
	Idle      time.Duration
	IdleKnown bool
	Students  []todayStudent
}

type visitRow struct {
	hours.Visit
	Student core.Student
}

type todayData struct {
	Hosts         []todayHost
	Live          int
	NoCheckin     int
	LastHeartbeat time.Time
	LastScan      time.Time
	Bot           *bot.Status
	OpenVisits    int
	Visits        []visitRow
}

func (s *Server) todayData(ctx context.Context) (*todayData, error) {
	now := s.Now()
	d := &todayData{}
	sessions := map[string]*todayHost{}
	sessByID := map[int64]*todayHost{}
	rows, err := s.DB.QueryContext(ctx, "SELECT id, host, started_at, last_seen_at, last_idle_ms FROM sessions WHERE ended_at IS NULL")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, started, seen int64
		var host string
		var idle sql.NullInt64
		if err := rows.Scan(&id, &host, &started, &seen, &idle); err != nil {
			rows.Close()
			return nil, err
		}
		h := &todayHost{Name: host, Live: true, Started: store.FromMs(started), LastSeen: store.FromMs(seen),
			Idle: time.Duration(idle.Int64) * time.Millisecond, IdleKnown: idle.Valid}
		h.Stale = now.Sub(h.LastSeen) > 150*time.Second
		sessions[host], sessByID[id] = h, h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT p.session_id, st.id, st.name, p.confirmed_at FROM presence p
		JOIN students st ON st.id = p.student_id WHERE p.ended_at IS NULL ORDER BY p.confirmed_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sid, at int64
		var ts todayStudent
		if err := rows.Scan(&sid, &ts.ID, &ts.Name, &at); err != nil {
			rows.Close()
			return nil, err
		}
		ts.Since = store.FromMs(at)
		d.OpenVisits++
		if h := sessByID[sid]; h != nil {
			h.Students = append(h.Students, ts)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lastEver := map[string]time.Time{}
	rows, err = s.DB.QueryContext(ctx, "SELECT host, MAX(last_seen_at) FROM sessions GROUP BY host")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host string
		var at int64
		if err := rows.Scan(&host, &at); err != nil {
			rows.Close()
			return nil, err
		}
		lastEver[host] = store.FromMs(at)
	}
	rows.Close()
	seen := map[string]bool{}
	for _, hst := range s.Hosts.All() {
		seen[hst.Name] = true
		if h := sessions[hst.Name]; h != nil {
			d.Hosts = append(d.Hosts, *h)
		} else {
			d.Hosts = append(d.Hosts, todayHost{Name: hst.Name, LastEver: lastEver[hst.Name]})
		}
	}
	for name, h := range sessions { // sessions on PCs removed from the hosts file
		if !seen[name] {
			d.Hosts = append(d.Hosts, *h)
		}
	}
	for _, h := range d.Hosts {
		if h.Live {
			d.Live++
			if len(h.Students) == 0 {
				d.NoCheckin++
			}
		}
	}
	var lastHB, lastScan sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(last_seen_at) FROM sessions").Scan(&lastHB); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(confirmed_at) FROM presence").Scan(&lastScan); err != nil {
		return nil, err
	}
	if lastHB.Valid {
		d.LastHeartbeat = store.FromMs(lastHB.Int64)
	}
	if lastScan.Valid {
		d.LastScan = store.FromMs(lastScan.Int64)
	}
	if s.Bot != nil {
		st := s.Bot.Status()
		d.Bot = &st
	}
	p := s.Calc.PeriodAt("day", now)
	visits, err := hours.Load(ctx, s.DB, p.From, p.To, 0)
	if err != nil {
		return nil, err
	}
	students, err := s.loadStudents(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range hours.Effective(visits, p.From, p.To) {
		d.Visits = append(d.Visits, visitRow{v, students[v.StudentID]})
	}
	sort.Slice(d.Visits, func(i, j int) bool { return d.Visits[i].Start.After(d.Visits[j].Start) })
	return d, nil
}

func (s *Server) handleToday(w http.ResponseWriter, r *http.Request) {
	d, err := s.todayData(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "today", d)
}

func (s *Server) handleTodayLive(w http.ResponseWriter, r *http.Request) {
	d, err := s.todayData(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.renderBlock(w, r, http.StatusOK, "today", "live", d)
}

// ---------- Hours ----------

type hoursRow struct {
	Student core.Student
	Row     hours.Row
	Days    []time.Duration
}

type hoursData struct {
	Period    hours.Period
	Date      string
	Group     string
	Groups    []string
	ShowEmpty bool
	DayCols   []time.Time
	Rows      []hoursRow
	Total     time.Duration
	Prev      string
	Next      string
	Cur       string
	CSV       string
	Policy    string
}

func (s *Server) parsePeriod(r *http.Request, defKind string) hours.Period {
	kind := r.URL.Query().Get("period")
	if kind != "day" && kind != "week" && kind != "month" {
		kind = defKind
	}
	at := s.Now()
	if d, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), s.Cfg.Timezone); err == nil {
		at = d.Add(12 * time.Hour)
	}
	return s.Calc.PeriodAt(kind, at)
}

func periodQuery(q url.Values, p hours.Period) string {
	q2 := url.Values{}
	for k, v := range q {
		q2[k] = v
	}
	q2.Set("period", p.Kind)
	q2.Set("date", p.From.Format("2006-01-02"))
	return q2.Encode()
}

func (s *Server) policyText() string {
	p := s.Calc.Policy
	var parts []string
	if p.MinVisit > 0 {
		parts = append(parts, s.t("hours.min_visit", i18n.Duration(s.Cfg.Lang, p.MinVisit)))
	}
	if p.Rounding > 0 {
		parts = append(parts, s.t("hours.rounding", i18n.Duration(s.Cfg.Lang, p.Rounding)))
	}
	if p.DailyCap > 0 {
		parts = append(parts, s.t("hours.cap", i18n.Duration(s.Cfg.Lang, p.DailyCap)))
	}
	if len(parts) == 0 {
		return s.t("hours.policy_exact")
	}
	return strings.Join(parts, "; ")
}

func (s *Server) hoursData(r *http.Request) (*hoursData, error) {
	ctx := r.Context()
	q := r.URL.Query()
	p := s.parsePeriod(r, "week")
	d := &hoursData{Period: p, Date: p.From.Format("2006-01-02"), Group: q.Get("group"), ShowEmpty: q.Get("empty") == "1", Policy: s.policyText()}
	var err error
	if d.Groups, err = s.groups(ctx); err != nil {
		return nil, err
	}
	for day := p.From; day.Before(p.To); day = s.Calc.Shift(hours.Period{Kind: "day", From: day}, 1).From {
		d.DayCols = append(d.DayCols, day)
	}
	visits, err := hours.Load(ctx, s.DB, p.From, p.To, 0)
	if err != nil {
		return nil, err
	}
	totals := s.Calc.Summarize(visits, p.From, p.To)
	students, err := s.loadStudents(ctx)
	if err != nil {
		return nil, err
	}
	for id, st := range students {
		if d.Group != "" && st.Group != d.Group {
			continue
		}
		row, ok := totals[id]
		if !ok {
			if !d.ShowEmpty || st.Archived {
				continue
			}
			row = &hours.Row{StudentID: id}
		}
		hr := hoursRow{Student: st, Row: *row}
		for _, day := range d.DayCols {
			hr.Days = append(hr.Days, row.Days[day.Format("2006-01-02")])
		}
		d.Rows = append(d.Rows, hr)
		d.Total += row.Total
	}
	sort.Slice(d.Rows, func(i, j int) bool {
		a, b := d.Rows[i].Student, d.Rows[j].Student
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Name < b.Name
	})
	d.Prev = periodQuery(q, s.Calc.Shift(p, -1))
	d.Next = periodQuery(q, s.Calc.Shift(p, 1))
	d.Cur = periodQuery(q, s.Calc.PeriodAt(p.Kind, s.Now()))
	d.CSV = periodQuery(q, p)
	return d, nil
}

func (s *Server) handleHours(w http.ResponseWriter, r *http.Request) {
	d, err := s.hoursData(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "hours", d)
}

// ---------- Students ----------

type studentRow struct {
	core.Student
	Linked      bool
	TGName      string
	TGUsername  string
	TGUserID    int64
	LinkedAt    time.Time
	CodeState   string // pending, expired or ""
	CodeExpires time.Time
}

type studentsData struct {
	Rows                              []studentRow
	Groups                            []string
	Group                             string
	Archived                          bool
	Total, Linked, Pending, NotLinked int
}

func (s *Server) studentRows(ctx context.Context, where string, args ...any) ([]studentRow, error) {
	now := s.Now()
	codes := map[int64]time.Time{}
	crows, err := s.DB.QueryContext(ctx, "SELECT student_id, MAX(expires_at) FROM enroll_codes WHERE used_at IS NULL AND revoked_at IS NULL GROUP BY student_id")
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var id, exp int64
		if err := crows.Scan(&id, &exp); err != nil {
			crows.Close()
			return nil, err
		}
		codes[id] = store.FromMs(exp)
	}
	crows.Close()
	rows, err := s.DB.QueryContext(ctx, `SELECT s.id, s.name, s.grp, s.archived, l.tg_user_id, COALESCE(l.tg_name, ''), COALESCE(l.tg_username, ''), COALESCE(l.linked_at, 0)
		FROM students s LEFT JOIN telegram_links l ON l.student_id = s.id WHERE `+where+` ORDER BY s.grp, s.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []studentRow
	for rows.Next() {
		var sr studentRow
		var tg sql.NullInt64
		var linked int64
		if err := rows.Scan(&sr.ID, &sr.Name, &sr.Group, &sr.Archived, &tg, &sr.TGName, &sr.TGUsername, &linked); err != nil {
			return nil, err
		}
		if tg.Valid {
			sr.Linked, sr.TGUserID, sr.LinkedAt = true, tg.Int64, store.FromMs(linked)
		} else if exp, ok := codes[sr.ID]; ok {
			sr.CodeExpires = exp
			sr.CodeState = "pending"
			if now.After(exp) {
				sr.CodeState = "expired"
			}
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

func (s *Server) handleStudents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &studentsData{Group: q.Get("group"), Archived: q.Get("archived") == "1"}
	where, args := "s.archived = ?", []any{d.Archived}
	if d.Group != "" {
		where += " AND s.grp = ?"
		args = append(args, d.Group)
	}
	var err error
	if d.Rows, err = s.studentRows(ctx, where, args...); err != nil {
		s.serverError(w, err)
		return
	}
	if d.Groups, err = s.groups(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	for _, sr := range d.Rows {
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
	s.render(w, r, http.StatusOK, "students", d)
}

type slip struct {
	Student core.Student
	Link    string
	Expires time.Time
}

// handleSlips shows printable personal links for students with an unused code.
func (s *Server) handleSlips(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	where, args := "c.used_at IS NULL AND c.revoked_at IS NULL AND c.expires_at > ? AND s.archived = 0", []any{store.Ms(s.Now())}
	if g := q.Get("group"); g != "" {
		where += " AND s.grp = ?"
		args = append(args, g)
	}
	if id, err := strconv.ParseInt(q.Get("id"), 10, 64); err == nil {
		where += " AND s.id = ?"
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT s.id, s.name, s.grp, c.code, c.expires_at FROM enroll_codes c
		JOIN students s ON s.id = c.student_id WHERE `+where+` ORDER BY s.grp, s.name`, args...)
	if err != nil {
		s.serverError(w, err)
		return
	}
	defer rows.Close()
	var slips []slip
	for rows.Next() {
		var sl slip
		var code string
		var exp int64
		if err := rows.Scan(&sl.Student.ID, &sl.Student.Name, &sl.Student.Group, &code, &exp); err != nil {
			s.serverError(w, err)
			return
		}
		sl.Link, sl.Expires = core.StartLink(s.Cfg.BotUsername, code), store.FromMs(exp)
		slips = append(slips, sl)
	}
	if err := rows.Err(); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "slips", slips)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func studentURL(id int64) string { return fmt.Sprintf("/admin/students/%d", id) }

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("file")
	if err != nil {
		back(w, r, "/admin/students", s.t("c.error", err.Error()), true)
		return
	}
	defer f.Close()
	res, err := core.ImportStudents(r.Context(), s.DB, s.Now(), f, staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/students", s.t("c.error", err.Error()), true)
		return
	}
	msg := s.t("students.import_done", res.Added, res.Updated, res.Unchanged)
	if len(res.Warnings) > 0 {
		n := min(len(res.Warnings), 5)
		msg += " " + strings.Join(res.Warnings[:n], "; ")
		if len(res.Warnings) > n {
			msg += fmt.Sprintf(" (+%d)", len(res.Warnings)-n)
		}
	}
	back(w, r, "/admin/students", msg, len(res.Warnings) > 0)
}

func (s *Server) handleAddStudent(w http.ResponseWriter, r *http.Request) {
	_, err := core.AddStudent(r.Context(), s.DB, s.Now(), r.PostFormValue("name"), r.PostFormValue("group"), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/students", s.t("c.error", err.Error()), true)
		return
	}
	back(w, r, "/admin/students", s.t("c.done"), false)
}

func (s *Server) handleEditStudent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := core.UpdateStudent(r.Context(), s.DB, s.Now(), id, r.PostFormValue("name"), r.PostFormValue("group"), staffFrom(r).Username)
	if err != nil {
		back(w, r, studentURL(id), s.t("c.error", err.Error()), true)
		return
	}
	back(w, r, studentURL(id), s.t("c.done"), false)
}

func (s *Server) handleGenCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := core.GenerateCode(r.Context(), s.DB, s.Now(), id, s.Cfg.EnrollCodeTTL, staffFrom(r).Username); err != nil {
		back(w, r, "/admin/students", s.t("c.error", err.Error()), true)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/students/slips?id=%d", id), http.StatusSeeOther)
}

// handleBulkCodes issues links for every active student (optionally in one
// group) who is not linked and has no valid unused link yet.
func (s *Server) handleBulkCodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	group := r.PostFormValue("group")
	where, args := "s.archived = 0", []any{}
	if group != "" {
		where += " AND s.grp = ?"
		args = append(args, group)
	}
	rows, err := s.studentRows(ctx, where, args...)
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, sr := range rows {
		if sr.Linked || sr.CodeState == "pending" {
			continue
		}
		if _, err := core.GenerateCode(ctx, s.DB, s.Now(), sr.ID, s.Cfg.EnrollCodeTTL, staffFrom(r).Username); err != nil {
			back(w, r, "/admin/students", s.t("c.error", err.Error()), true)
			return
		}
	}
	http.Redirect(w, r, "/admin/students/slips?group="+url.QueryEscape(group), http.StatusSeeOther)
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		back(w, r, studentURL(id), s.t("c.error", core.ErrReasonRequired.Error()), true)
		return
	}
	if err := core.Unlink(r.Context(), s.DB, s.Now(), id, staffFrom(r).Username, reason); err != nil {
		back(w, r, studentURL(id), s.t("c.error", err.Error()), true)
		return
	}
	back(w, r, studentURL(id), s.t("c.done"), false)
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	archived := r.PostFormValue("archived") == "1"
	if err := core.SetArchived(r.Context(), s.DB, s.Now(), id, archived, staffFrom(r).Username); err != nil {
		back(w, r, studentURL(id), s.t("c.error", err.Error()), true)
		return
	}
	back(w, r, studentURL(id), s.t("c.done"), false)
}

// ---------- Student detail and corrections ----------

type studentData struct {
	Student     studentRow
	Period      hours.Period
	Prev, Next  string
	Visits      []hours.Visit
	Total       time.Duration
	Days        []dayTotal
	Corrections []hours.Correction
	Hosts       []string
	NowInput    time.Time
	LatencyMs   int64 // scans slower than this are flagged
}

type dayTotal struct {
	Day   time.Time
	Total time.Duration
}

func (s *Server) handleStudent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rows, err := s.studentRows(ctx, "s.id = ?", id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(rows) == 0 {
		http.NotFound(w, r)
		return
	}
	p := s.parsePeriod(r, "month")
	d := &studentData{Student: rows[0], Period: p, NowInput: s.Now(), LatencyMs: s.Cfg.AnomalyLatency.Milliseconds()}
	d.Prev = periodQuery(url.Values{}, s.Calc.Shift(p, -1))
	d.Next = periodQuery(url.Values{}, s.Calc.Shift(p, 1))
	visits, err := hours.Load(ctx, s.DB, p.From, p.To, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	sort.Slice(visits, func(i, j int) bool { return visits[i].Start.Before(visits[j].Start) })
	d.Visits = visits
	if row := s.Calc.Summarize(visits, p.From, p.To)[id]; row != nil {
		d.Total = row.Total
		for day := p.From; day.Before(p.To); day = s.Calc.Shift(hours.Period{Kind: "day", From: day}, 1).From {
			if t, ok := row.Days[day.Format("2006-01-02")]; ok {
				d.Days = append(d.Days, dayTotal{day, t})
			}
		}
	}
	if d.Corrections, err = hours.LoadCorrections(ctx, s.DB, id); err != nil {
		s.serverError(w, err)
		return
	}
	for _, h := range s.Hosts.All() {
		d.Hosts = append(d.Hosts, h.Name)
	}
	s.render(w, r, http.StatusOK, "student", d)
}

func (s *Server) parseLocal(v string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02T15:04", v, s.Cfg.Timezone)
}

func (s *Server) handleCorrection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	in := core.CorrectionInput{
		Action:    r.PostFormValue("action"),
		Target:    r.PostFormValue("target"),
		StudentID: id,
		Host:      r.PostFormValue("host"),
		Reason:    r.PostFormValue("reason"),
		Actor:     staffFrom(r).Username,
	}
	if in.Action != "delete" {
		var err1, err2 error
		in.Start, err1 = s.parseLocal(r.PostFormValue("start"))
		in.End, err2 = s.parseLocal(r.PostFormValue("end"))
		if err := errors.Join(err1, err2); err != nil {
			back(w, r, studentURL(id)+"?"+r.URL.RawQuery, s.t("c.error", core.ErrBadTimes.Error()), true)
			return
		}
	}
	if _, err := core.AddCorrection(r.Context(), s.DB, s.Now(), in); err != nil {
		back(w, r, studentURL(id)+"?"+r.URL.RawQuery, s.t("c.error", err.Error()), true)
		return
	}
	back(w, r, studentURL(id)+"?"+r.URL.RawQuery, s.t("c.done"), false)
}

// ---------- Anomalies ----------

type slowScan struct {
	Student   core.Student
	Host      string
	At        time.Time
	LatencyMs int64
	Stale     bool
}

type multiPC struct {
	Student               core.Student
	FirstHost, SecondHost string
	FirstAt, SecondAt     time.Time
	FirstActiveUntil      time.Time
}

type manySession struct {
	Host     string
	Started  time.Time
	Students []string
}

type failedScan struct {
	At      time.Time
	TGUser  int64
	Student string
	Host    string
	Result  string
}

type anomData struct {
	From, To string
	Latency  time.Duration
	Window   time.Duration
	Slow     []slowScan
	Multi    []multiPC
	Many     []manySession
	Auto     []visitRow
	Failed   []failedScan
}

func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	d, err := s.anomalies(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "anomalies", d)
}

func (s *Server) anomalies(r *http.Request) (*anomData, error) {
	ctx := r.Context()
	now := s.Now()
	loc := s.Cfg.Timezone
	to := s.Calc.PeriodAt("day", now).To
	from := to.AddDate(0, 0, -7)
	if t, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), loc); err == nil {
		from = t
	}
	if t, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), loc); err == nil {
		to = t.AddDate(0, 0, 1)
	}
	d := &anomData{From: from.Format("2006-01-02"), To: to.AddDate(0, 0, -1).Format("2006-01-02"),
		Latency: s.Cfg.AnomalyLatency, Window: s.Cfg.AnomalyMultiPC}
	students, err := s.loadStudents(ctx)
	if err != nil {
		return nil, err
	}
	f, t := store.Ms(from), store.Ms(to)

	rows, err := s.DB.QueryContext(ctx, `SELECT student_id, host, confirmed_at, scan_latency_ms, stale_token FROM presence
		WHERE confirmed_at >= ? AND confirmed_at < ? AND (scan_latency_ms > ? OR stale_token = 1) ORDER BY confirmed_at DESC`,
		f, t, s.Cfg.AnomalyLatency.Milliseconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sc slowScan
		var sid, at int64
		if err := rows.Scan(&sid, &sc.Host, &at, &sc.LatencyMs, &sc.Stale); err != nil {
			rows.Close()
			return nil, err
		}
		sc.Student, sc.At = students[sid], store.FromMs(at)
		d.Slow = append(d.Slow, sc)
	}
	rows.Close()

	// One student on several PCs: consecutive scans on different PCs within
	// the window while the first PC kept being used after the second scan.
	rows, err = s.DB.QueryContext(ctx, `SELECT p.student_id, p.host, p.confirmed_at, s.last_activity_at FROM presence p
		JOIN sessions s ON s.id = p.session_id WHERE p.confirmed_at >= ? AND p.confirmed_at < ? ORDER BY p.student_id, p.confirmed_at`,
		store.Ms(from.Add(-s.Cfg.AnomalyMultiPC)), t)
	if err != nil {
		return nil, err
	}
	type scanRow struct {
		student      int64
		host         string
		at, activity time.Time
	}
	var prev *scanRow
	for rows.Next() {
		var cur scanRow
		var at, act int64
		if err := rows.Scan(&cur.student, &cur.host, &at, &act); err != nil {
			rows.Close()
			return nil, err
		}
		cur.at, cur.activity = store.FromMs(at), store.FromMs(act)
		if prev != nil && prev.student == cur.student && prev.host != cur.host &&
			cur.at.Sub(prev.at) <= s.Cfg.AnomalyMultiPC && prev.activity.After(cur.at.Add(2*time.Minute)) && !cur.at.Before(from) {
			d.Multi = append(d.Multi, multiPC{Student: students[cur.student], FirstHost: prev.host, FirstAt: prev.at,
				SecondHost: cur.host, SecondAt: cur.at, FirstActiveUntil: prev.activity})
		}
		c := cur
		prev = &c
	}
	rows.Close()

	rows, err = s.DB.QueryContext(ctx, `SELECT p.session_id, s.host, s.started_at, GROUP_CONCAT(DISTINCT p.student_id) FROM presence p
		JOIN sessions s ON s.id = p.session_id WHERE p.confirmed_at >= ? AND p.confirmed_at < ?
		GROUP BY p.session_id HAVING COUNT(DISTINCT p.student_id) > 1 ORDER BY s.started_at DESC`, f, t)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ms manySession
		var sid, started int64
		var ids string
		if err := rows.Scan(&sid, &ms.Host, &started, &ids); err != nil {
			rows.Close()
			return nil, err
		}
		ms.Started = store.FromMs(started)
		for _, x := range strings.Split(ids, ",") {
			n, _ := strconv.ParseInt(x, 10, 64)
			ms.Students = append(ms.Students, students[n].Name)
		}
		d.Many = append(d.Many, ms)
	}
	rows.Close()

	visits, err := hours.Load(ctx, s.DB, from, to, 0)
	if err != nil {
		return nil, err
	}
	for _, v := range visits {
		if v.EndReason == core.ReasonAutoClosed && !v.Deleted {
			d.Auto = append(d.Auto, visitRow{v, students[v.StudentID]})
		}
	}
	sort.Slice(d.Auto, func(i, j int) bool { return d.Auto[i].Start.After(d.Auto[j].Start) })

	rows, err = s.DB.QueryContext(ctx, `SELECT e.at, COALESCE(e.tg_user_id, 0), e.host, e.result, COALESCE(st.name, '')
		FROM events e LEFT JOIN telegram_links l ON l.tg_user_id = e.tg_user_id LEFT JOIN students st ON st.id = l.student_id
		WHERE e.kind = 'scan' AND e.at >= ? AND e.at < ? AND e.result NOT IN ('ok', 'already')
		ORDER BY e.at DESC LIMIT 300`, f, t)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fs failedScan
		var at int64
		if err := rows.Scan(&at, &fs.TGUser, &fs.Host, &fs.Result, &fs.Student); err != nil {
			rows.Close()
			return nil, err
		}
		fs.At = store.FromMs(at)
		d.Failed = append(d.Failed, fs)
	}
	rows.Close()
	return d, rows.Err()
}

// ---------- Audit ----------

type auditRow struct {
	At                             time.Time
	Actor, Action, Target, Details string
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.QueryContext(r.Context(), "SELECT at, actor, action, target, details FROM audit_log ORDER BY id DESC LIMIT 500")
	if err != nil {
		s.serverError(w, err)
		return
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var at int64
		if err := rows.Scan(&at, &a.Actor, &a.Action, &a.Target, &a.Details); err != nil {
			s.serverError(w, err)
			return
		}
		a.At = store.FromMs(at)
		out = append(out, a)
	}
	s.render(w, r, http.StatusOK, "audit", out)
}
