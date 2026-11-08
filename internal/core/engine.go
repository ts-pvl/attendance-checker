// Package core holds the attendance rules: PC sessions, visits (presence),
// check-in, check-out, enrollment and the nightly cleanup.
//
// Every input is first stored as a raw row in the events table and then
// applied by Engine.apply. Applying the stored events in id order rebuilds
// sessions and presence exactly (see Replay), so rules can change and
// history can be recomputed.
//
// Rules, all on the server clock:
//   - A session is dead once no event has arrived from it for longer than
//     the heartbeat timeout. It is closed at its last event; its visits close
//     at the session's last activity (last heartbeat minus reported idle).
//   - Idle logout / logout close the session; visits close at last activity.
//   - A student has at most one open visit. A scan on another PC closes the
//     earlier visit at that PC's last activity.
//   - A PC session has at most one student. A scan by another student on the
//     same PC closes the earlier student's visit at the PC's last activity.
//   - A visit never ends before it started.
package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"attendance/internal/store"
)

// Event kinds.
const (
	KindSessionStart = "session_start"
	KindHeartbeat    = "heartbeat"
	KindIdleLogout   = "idle_logout"
	KindLogout       = "logout"
	KindScan         = "scan"
	KindCheckout     = "checkout"
	KindCleanup      = "cleanup"
	KindEnroll       = "enroll"
)

// End reasons for sessions and visits.
const (
	ReasonIdleLogout = "idle_logout"
	ReasonLogout     = "logout"
	ReasonTimeout    = "timeout"
	ReasonReplaced   = "replaced" // a new login on the PC arrived without an end event
	ReasonAutoClosed = "auto_closed"
	ReasonCheckout   = "checkout"
	ReasonMoved      = "moved"      // the student checked in on another PC
	ReasonTakenOver  = "taken_over" // another student checked in on this PC
	ReasonManual     = "manual"
)

// Scan results stored in events.result.
const (
	ScanOK          = "ok"
	ScanAlready     = "already"
	ScanNoSession   = "no_session"
	ScanBadToken    = "bad_token"
	ScanExpired     = "expired"
	ScanUsed        = "used"
	ScanUnknownHost = "unknown_host"
	ScanNotLinked   = "not_linked"
)

// maxIdle bounds a reported idle time; anything larger is treated as unknown.
const maxIdle = 7 * 24 * time.Hour

// Event is one raw input.
type Event struct {
	ID        int64
	At        time.Time
	Kind      string
	Host      string
	SID       string
	IdleMs    sql.NullInt64
	TGUserID  int64
	StudentID int64
	Detail    EventDetail
	Result    string
}

// EventDetail holds kind-specific fields, stored as JSON.
type EventDetail struct {
	Nonce     string `json:"nonce,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Stale     bool   `json:"stale,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Closure describes a visit that was just closed.
type Closure struct {
	PresenceID int64
	StudentID  int64
	Host       string
	Start, End time.Time
	Reason     string
}

type Engine struct {
	mu            sync.Mutex
	db            *store.DB
	timeout       time.Duration
	checkoutSlack time.Duration
}

func NewEngine(db *store.DB, heartbeatTimeout, checkoutSlack time.Duration) *Engine {
	return &Engine{db: db, timeout: heartbeatTimeout, checkoutSlack: checkoutSlack}
}

func (e *Engine) DB() *store.DB { return e.db }

// Timeout returns the heartbeat timeout.
func (e *Engine) Timeout() time.Duration { return e.timeout }

// record inserts ev into the events table and sets ev.ID.
func record(ctx context.Context, tx *sql.Tx, ev *Event) error {
	detail := ""
	if ev.Detail != (EventDetail{}) {
		b, _ := json.Marshal(ev.Detail)
		detail = string(b)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events(at, kind, host, sid, idle_ms, tg_user_id, student_id, detail, result)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		store.Ms(ev.At), ev.Kind, ev.Host, ev.SID, ev.IdleMs, nullID(ev.TGUserID), nullID(ev.StudentID), detail, ev.Result)
	if err != nil {
		return err
	}
	ev.ID, err = res.LastInsertId()
	return err
}

func setResult(ctx context.Context, tx *sql.Tx, id int64, result string) error {
	_, err := tx.ExecContext(ctx, "UPDATE events SET result = ? WHERE id = ?", result, id)
	return err
}

func nullID(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: id != 0} }

// submit records ev and applies it in one transaction.
func (e *Engine) submit(ctx context.Context, ev *Event) ([]Closure, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Closure
	err := e.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := record(ctx, tx, ev); err != nil {
			return err
		}
		cl, result, err := e.apply(ctx, tx, ev)
		if err != nil {
			return err
		}
		out = cl
		ev.Result = result
		return setResult(ctx, tx, ev.ID, result)
	})
	return out, err
}

// recordOnly stores an event that is not applied (for example a rejected scan).
func (e *Engine) recordOnly(ctx context.Context, ev *Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db.InTx(ctx, func(tx *sql.Tx) error { return record(ctx, tx, ev) })
}

// apply is the single place where events change sessions and presence.
func (e *Engine) apply(ctx context.Context, tx *sql.Tx, ev *Event) ([]Closure, string, error) {
	// Sessions that went silent before this event are dead regardless of
	// whether the periodic sweep has run yet; this keeps replay identical.
	closures, err := e.sweep(ctx, tx, ev.At)
	if err != nil {
		return nil, "", err
	}
	var more []Closure
	var result string
	switch ev.Kind {
	case KindSessionStart, KindHeartbeat, KindIdleLogout, KindLogout:
		more, result, err = e.applyAgent(ctx, tx, ev)
	case KindScan:
		more, result, err = e.applyScan(ctx, tx, ev)
	case KindCheckout:
		more, result, err = e.applyCheckout(ctx, tx, ev)
	case KindCleanup:
		more, result, err = e.applyCleanup(ctx, tx, ev)
	default:
		return nil, "", fmt.Errorf("core: cannot apply event kind %q", ev.Kind)
	}
	return append(closures, more...), result, err
}

type session struct {
	ID           int64
	Host, SID    string
	Started      time.Time
	LastSeen     time.Time
	LastActivity time.Time
}

const sessionCols = "id, host, sid, started_at, last_seen_at, last_activity_at"

func scanSession(row interface{ Scan(...any) error }) (*session, error) {
	var s session
	var started, seen, act int64
	if err := row.Scan(&s.ID, &s.Host, &s.SID, &started, &seen, &act); err != nil {
		return nil, err
	}
	s.Started, s.LastSeen, s.LastActivity = store.FromMs(started), store.FromMs(seen), store.FromMs(act)
	return &s, nil
}

func openSession(ctx context.Context, tx *sql.Tx, host string) (*session, error) {
	s, err := scanSession(tx.QueryRowContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE host = ? AND ended_at IS NULL", host))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func sessionByID(ctx context.Context, tx *sql.Tx, id int64) (*session, error) {
	return scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
}

// sweep closes sessions that have been silent for longer than the timeout as of now.
func (e *Engine) sweep(ctx context.Context, tx *sql.Tx, now time.Time) ([]Closure, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE ended_at IS NULL AND last_seen_at < ? ORDER BY id",
		store.Ms(now.Add(-e.timeout)))
	if err != nil {
		return nil, err
	}
	var dead []*session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		dead = append(dead, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Closure
	for _, s := range dead {
		cl, err := endSession(ctx, tx, s, s.LastSeen, ReasonTimeout)
		if err != nil {
			return nil, err
		}
		out = append(out, cl...)
	}
	return out, nil
}

// endSession closes s at endedAt and closes its open visits at the session's
// last activity (but never before a visit started or after endedAt).
func endSession(ctx context.Context, tx *sql.Tx, s *session, endedAt time.Time, reason string) ([]Closure, error) {
	res, err := tx.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
		store.Ms(endedAt), reason, s.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // already closed: closing twice changes nothing
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT id, student_id, confirmed_at FROM presence WHERE session_id = ? AND ended_at IS NULL ORDER BY id", s.ID)
	if err != nil {
		return nil, err
	}
	type open struct {
		id, student int64
		confirmed   time.Time
	}
	var opens []open
	for rows.Next() {
		var o open
		var c int64
		if err := rows.Scan(&o.id, &o.student, &c); err != nil {
			rows.Close()
			return nil, err
		}
		o.confirmed = store.FromMs(c)
		opens = append(opens, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Closure
	for _, o := range opens {
		end := clamp(s.LastActivity, o.confirmed, endedAt)
		if err := closePresence(ctx, tx, o.id, end, reason); err != nil {
			return nil, err
		}
		out = append(out, Closure{PresenceID: o.id, StudentID: o.student, Host: s.Host, Start: o.confirmed, End: end, Reason: reason})
	}
	return out, nil
}

func closePresence(ctx context.Context, tx *sql.Tx, id int64, end time.Time, reason string) error {
	_, err := tx.ExecContext(ctx, "UPDATE presence SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
		store.Ms(end), reason, id)
	return err
}

// clamp returns t limited to [lo, hi]; if hi < lo the result is lo.
func clamp(t, lo, hi time.Time) time.Time {
	if t.After(hi) {
		t = hi
	}
	if t.Before(lo) {
		t = lo
	}
	return t
}

func idleOf(ev *Event) (time.Duration, bool) {
	if !ev.IdleMs.Valid || ev.IdleMs.Int64 < 0 {
		return 0, false
	}
	d := time.Duration(ev.IdleMs.Int64) * time.Millisecond
	if d > maxIdle {
		return 0, false
	}
	return d, true
}

// touch records that s was seen at ev.At and, if the idle time is known,
// moves its last activity forward. A heartbeat without idle time keeps the
// session alive but says nothing about activity.
func touch(ctx context.Context, tx *sql.Tx, s *session, ev *Event) error {
	s.LastSeen = ev.At
	if idle, ok := idleOf(ev); ok {
		if act := ev.At.Add(-idle); act.After(s.LastActivity) {
			s.LastActivity = act
		}
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE sessions SET last_seen_at = ?, last_activity_at = ?, last_idle_ms = COALESCE(?, last_idle_ms) WHERE id = ?",
		store.Ms(s.LastSeen), store.Ms(s.LastActivity), ev.IdleMs, s.ID)
	return err
}

func (e *Engine) applyAgent(ctx context.Context, tx *sql.Tx, ev *Event) ([]Closure, string, error) {
	s, err := openSession(ctx, tx, ev.Host)
	if err != nil {
		return nil, "", err
	}
	same := s != nil && s.SID == ev.SID
	switch ev.Kind {
	case KindIdleLogout, KindLogout:
		if !same {
			return nil, "no_open_session", nil
		}
		if err := touch(ctx, tx, s, ev); err != nil {
			return nil, "", err
		}
		cl, err := endSession(ctx, tx, s, ev.At, ev.Kind)
		return cl, "ended", err
	}
	// session_start or heartbeat
	if same {
		if ev.Kind == KindSessionStart {
			// The agent restarted inside the same login; keep the session.
			ev.IdleMs = sql.NullInt64{}
		}
		return nil, "ok", touch(ctx, tx, s, ev)
	}
	var closures []Closure
	if s != nil {
		// A different login on this PC while the old one looked alive: the old
		// one ended without telling us (crash, reboot within the timeout).
		closures, err = endSession(ctx, tx, s, s.LastSeen, ReasonReplaced)
		if err != nil {
			return nil, "", err
		}
	}
	implicit := ev.Kind == KindHeartbeat // we missed session_start (server was down, or a timeout)
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id, host, sid, started_at, last_seen_at, last_activity_at, last_idle_ms, implicit)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Host, ev.SID, store.Ms(ev.At), store.Ms(ev.At), store.Ms(ev.At), ev.IdleMs, implicit)
	if err != nil {
		return nil, "", err
	}
	if implicit {
		return closures, "started_implicit", nil
	}
	return closures, "started", nil
}

type openVisit struct {
	ID, SessionID int64
	Host          string
	Confirmed     time.Time
}

func openPresenceOf(ctx context.Context, tx *sql.Tx, studentID int64) (*openVisit, error) {
	var v openVisit
	var c int64
	err := tx.QueryRowContext(ctx,
		"SELECT id, session_id, host, confirmed_at FROM presence WHERE student_id = ? AND ended_at IS NULL", studentID).
		Scan(&v.ID, &v.SessionID, &v.Host, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	v.Confirmed = store.FromMs(c)
	return &v, err
}

func (e *Engine) applyScan(ctx context.Context, tx *sql.Tx, ev *Event) ([]Closure, string, error) {
	s, err := openSession(ctx, tx, ev.Host)
	if err != nil {
		return nil, "", err
	}
	if s == nil {
		return nil, ScanNoSession, nil
	}
	cur, err := openPresenceOf(ctx, tx, ev.StudentID)
	if err != nil {
		return nil, "", err
	}
	if cur != nil && cur.SessionID == s.ID {
		return nil, ScanAlready, nil
	}
	var closures []Closure
	if cur != nil {
		prev, err := sessionByID(ctx, tx, cur.SessionID)
		if err != nil {
			return nil, "", err
		}
		end := clamp(prev.LastActivity, cur.Confirmed, ev.At)
		if err := closePresence(ctx, tx, cur.ID, end, ReasonMoved); err != nil {
			return nil, "", err
		}
		closures = append(closures, Closure{PresenceID: cur.ID, StudentID: ev.StudentID, Host: cur.Host, Start: cur.Confirmed, End: end, Reason: ReasonMoved})
	}
	taken, err := takeOver(ctx, tx, s, ev)
	if err != nil {
		return nil, "", err
	}
	closures = append(closures, taken...)
	_, err = tx.ExecContext(ctx, `INSERT INTO presence(id, student_id, host, session_id, confirmed_at, scan_latency_ms, stale_token)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.StudentID, ev.Host, s.ID, store.Ms(ev.At), ev.Detail.LatencyMs, ev.Detail.Stale)
	if err != nil {
		return nil, "", err
	}
	return closures, ScanOK, nil
}

// takeOver closes the visits of other students on session s. Each ends at the
// PC's last activity, but never before it started or after this scan.
func takeOver(ctx context.Context, tx *sql.Tx, s *session, ev *Event) ([]Closure, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT id, student_id, confirmed_at FROM presence WHERE session_id = ? AND ended_at IS NULL AND student_id <> ? ORDER BY id",
		s.ID, ev.StudentID)
	if err != nil {
		return nil, err
	}
	var out []Closure
	for rows.Next() {
		c := Closure{Host: s.Host, Reason: ReasonTakenOver}
		var conf int64
		if err := rows.Scan(&c.PresenceID, &c.StudentID, &conf); err != nil {
			rows.Close()
			return nil, err
		}
		c.Start = store.FromMs(conf)
		c.End = clamp(s.LastActivity, c.Start, ev.At)
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		if err := closePresence(ctx, tx, c.PresenceID, c.End, ReasonTakenOver); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (e *Engine) applyCheckout(ctx context.Context, tx *sql.Tx, ev *Event) ([]Closure, string, error) {
	cur, err := openPresenceOf(ctx, tx, ev.StudentID)
	if err != nil {
		return nil, "", err
	}
	if cur == nil {
		return nil, "none", nil
	}
	s, err := sessionByID(ctx, tx, cur.SessionID)
	if err != nil {
		return nil, "", err
	}
	// The student says "I'm leaving now". Believe it only if the PC was in
	// use recently; otherwise they left earlier and the last activity counts.
	end := ev.At
	if ev.At.Sub(s.LastActivity) > e.checkoutSlack {
		end = s.LastActivity
	}
	end = clamp(end, cur.Confirmed, ev.At)
	if err := closePresence(ctx, tx, cur.ID, end, ReasonCheckout); err != nil {
		return nil, "", err
	}
	return []Closure{{PresenceID: cur.ID, StudentID: ev.StudentID, Host: cur.Host, Start: cur.Confirmed, End: end, Reason: ReasonCheckout}}, "ok", nil
}

// applyCleanup closes every open visit at its session's last activity.
// Sessions stay open; a student still working simply scans again.
func (e *Engine) applyCleanup(ctx context.Context, tx *sql.Tx, ev *Event) ([]Closure, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id, p.student_id, p.host, p.confirmed_at, s.last_activity_at
		FROM presence p JOIN sessions s ON s.id = p.session_id WHERE p.ended_at IS NULL ORDER BY p.id`)
	if err != nil {
		return nil, "", err
	}
	var out []Closure
	for rows.Next() {
		var c Closure
		var conf, act int64
		if err := rows.Scan(&c.PresenceID, &c.StudentID, &c.Host, &conf, &act); err != nil {
			rows.Close()
			return nil, "", err
		}
		c.Start = store.FromMs(conf)
		c.End = clamp(store.FromMs(act), c.Start, ev.At)
		c.Reason = ReasonAutoClosed
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	for _, c := range out {
		if err := closePresence(ctx, tx, c.PresenceID, c.End, ReasonAutoClosed); err != nil {
			return nil, "", err
		}
	}
	return out, fmt.Sprintf("closed %d", len(out)), nil
}

// AgentEvent processes a report from a PC agent.
func (e *Engine) AgentEvent(ctx context.Context, now time.Time, host, kind, sid string, idleMs sql.NullInt64) ([]Closure, error) {
	switch kind {
	case KindSessionStart, KindHeartbeat, KindIdleLogout, KindLogout:
	default:
		return nil, fmt.Errorf("core: unknown agent event %q", kind)
	}
	ev := &Event{At: now, Kind: kind, Host: host, SID: sid, IdleMs: idleMs}
	return e.submit(ctx, ev)
}

// Checkout closes the student's open visit on request (bot /checkout).
func (e *Engine) Checkout(ctx context.Context, now time.Time, tgUserID, studentID int64) (*Closure, error) {
	ev := &Event{At: now, Kind: KindCheckout, TGUserID: tgUserID, StudentID: studentID}
	cl, err := e.submit(ctx, ev)
	if err != nil {
		return nil, err
	}
	for i := range cl {
		if cl[i].Reason == ReasonCheckout {
			return &cl[i], nil
		}
	}
	return nil, nil
}

// Cleanup is the nightly job: it closes every visit that is still open.
func (e *Engine) Cleanup(ctx context.Context, now time.Time) ([]Closure, error) {
	return e.submit(ctx, &Event{At: now, Kind: KindCleanup})
}

// Sweep closes timed-out sessions. It records no event: the result is fully
// determined by the timestamps already stored.
func (e *Engine) Sweep(ctx context.Context, now time.Time) ([]Closure, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Closure
	err := e.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = e.sweep(ctx, tx, now)
		return err
	})
	return out, err
}

// replayable reports whether a stored event takes part in replay.
func replayable(kind, result string) bool {
	switch kind {
	case KindSessionStart, KindHeartbeat, KindIdleLogout, KindLogout, KindCheckout, KindCleanup:
		return true
	case KindScan:
		// Scans rejected before reaching the rules (bad token, unlinked...)
		// stay rejected; the rest are re-evaluated.
		return result == ScanOK || result == ScanAlready || result == ScanNoSession
	}
	return false
}

// Replay rebuilds sessions and presence from the events table. Corrections
// refer to presence by scan event id, so they survive a replay. Stop the
// server before running it.
func (e *Engine) Replay(ctx context.Context, now time.Time) (events int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	err = e.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM presence"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
			return err
		}
		var last int64
		for {
			batch, err := loadEvents(ctx, tx, last, 5000)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			for i := range batch {
				ev := &batch[i]
				last = ev.ID
				if !replayable(ev.Kind, ev.Result) {
					continue
				}
				if _, _, err := e.apply(ctx, tx, ev); err != nil {
					return fmt.Errorf("event %d: %w", ev.ID, err)
				}
				events++
			}
		}
		_, err := e.sweep(ctx, tx, now)
		return err
	})
	return events, err
}

func loadEvents(ctx context.Context, q store.Querier, afterID int64, limit int) ([]Event, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, at, kind, host, sid, idle_ms, COALESCE(tg_user_id, 0), COALESCE(student_id, 0), detail, result
		FROM events WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var at int64
		var detail string
		if err := rows.Scan(&ev.ID, &at, &ev.Kind, &ev.Host, &ev.SID, &ev.IdleMs, &ev.TGUserID, &ev.StudentID, &detail, &ev.Result); err != nil {
			return nil, err
		}
		ev.At = store.FromMs(at)
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &ev.Detail); err != nil {
				return nil, fmt.Errorf("event %d detail: %w", ev.ID, err)
			}
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
