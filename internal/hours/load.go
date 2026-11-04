package hours

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"attendance/internal/store"
)

// Load returns visits (raw plus corrections, deleted ones flagged) that
// intersect [from, to). studentID 0 means all students. The Hours screen,
// the student page, the bot and the CSV export all use this one query, so
// their numbers always match.
func Load(ctx context.Context, q store.Querier, from, to time.Time, studentID int64) ([]Visit, error) {
	corrs, err := LoadCorrections(ctx, q, studentID)
	if err != nil {
		return nil, err
	}
	where := "p.confirmed_at < ? AND MAX(p.confirmed_at, COALESCE(p.ended_at, s.last_activity_at)) >= ?"
	args := []any{store.Ms(to), store.Ms(from)}
	if studentID != 0 {
		where += " AND p.student_id = ?"
		args = append(args, studentID)
	}
	raw, err := loadPresence(ctx, q, where, args...)
	if err != nil {
		return nil, err
	}
	// Edits can move a visit into this range from anywhere, so load every
	// targeted visit that the range query missed.
	have := map[string]bool{}
	for _, v := range raw {
		have[v.Key] = true
	}
	var missing []string
	for _, c := range corrs {
		if strings.HasPrefix(c.Target, "p") && !have[c.Target] {
			if _, err := strconv.ParseInt(c.Target[1:], 10, 64); err == nil {
				missing = append(missing, c.Target[1:])
				have[c.Target] = true
			}
		}
	}
	for len(missing) > 0 {
		n := min(len(missing), 500)
		chunk := missing[:n]
		missing = missing[n:]
		more, err := loadPresence(ctx, q, "p.id IN ("+strings.Join(chunk, ",")+")")
		if err != nil {
			return nil, err
		}
		raw = append(raw, more...)
	}
	all := Apply(raw, corrs)
	out := all[:0]
	for _, v := range all {
		if Intersects(v, from, to) {
			out = append(out, v)
		}
	}
	return out, nil
}

func loadPresence(ctx context.Context, q store.Querier, where string, args ...any) ([]Visit, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id, p.student_id, p.host, p.session_id, p.confirmed_at, p.ended_at,
			COALESCE(p.end_reason, ''), p.scan_latency_ms, p.stale_token, s.last_activity_at
		FROM presence p JOIN sessions s ON s.id = p.session_id WHERE `+where+` ORDER BY p.confirmed_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Visit
	for rows.Next() {
		var v Visit
		var id, conf, lastAct int64
		var ended sql.NullInt64
		if err := rows.Scan(&id, &v.StudentID, &v.Host, &v.SessionID, &conf, &ended, &v.EndReason, &v.LatencyMs, &v.Stale, &lastAct); err != nil {
			return nil, err
		}
		v.Key = "p" + strconv.FormatInt(id, 10)
		v.Start = store.FromMs(conf)
		if ended.Valid {
			v.End = store.FromMs(ended.Int64)
		} else {
			v.Open = true
			v.End = maxTime(store.FromMs(lastAct), v.Start)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LoadCorrections returns corrections for one student (or all when 0), oldest first.
func LoadCorrections(ctx context.Context, q store.Querier, studentID int64) ([]Correction, error) {
	query := "SELECT id, action, target, student_id, host, COALESCE(start_at, 0), COALESCE(end_at, 0), reason, actor, created_at FROM corrections"
	var args []any
	if studentID != 0 {
		query += " WHERE student_id = ?"
		args = append(args, studentID)
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Correction
	for rows.Next() {
		var c Correction
		var s, e, created int64
		if err := rows.Scan(&c.ID, &c.Action, &c.Target, &c.StudentID, &c.Host, &s, &e, &c.Reason, &c.Actor, &created); err != nil {
			return nil, err
		}
		c.Start, c.End, c.Created = store.FromMs(s), store.FromMs(e), store.FromMs(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Today returns a student's total for the local day containing now, and
// whether a visit is open.
func (c Calc) Today(ctx context.Context, q store.Querier, studentID int64, now time.Time) (time.Duration, error) {
	p := c.PeriodAt("day", now)
	visits, err := Load(ctx, q, p.From, p.To, studentID)
	if err != nil {
		return 0, err
	}
	if r := c.Summarize(visits, p.From, p.To)[studentID]; r != nil {
		return r.Total, nil
	}
	return 0, nil
}
