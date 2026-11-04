package core

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"attendance/internal/store"
	"attendance/internal/token"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

// day is the date all scenarios run on (a Tuesday).
const day = "2026-03-10"

// T parses a wall-clock time on the scenario day, e.g. T("10:00:20").
// "+1 03:59" means the next day.
func T(s string) time.Time {
	d := day
	if len(s) > 3 && s[:3] == "+1 " {
		d, s = "2026-03-11", s[3:]
	}
	if len(s) == 5 {
		s += ":00"
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", d+" "+s, almaty)
	if err != nil {
		panic(err)
	}
	return t
}

type env struct {
	t       *testing.T
	ctx     context.Context
	db      *store.DB
	engine  *Engine
	checkin *Checkin
	hosts   *Hosts
}

const (
	alice = int64(1)
	bob   = int64(2)
	carol = int64(3)

	tgAlice = int64(1001)
	tgBob   = int64(1002)
	tgCarol = int64(1003)
	tgEve   = int64(6666) // not linked
)

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hosts := NewHosts("")
	var entries []HostEntry
	for i := 1; i <= 3; i++ {
		entries = append(entries, HostEntry{Name: fmt.Sprintf("PC-%02d", i), IP: netip.MustParseAddr(fmt.Sprintf("10.0.0.%d", 10+i))})
	}
	if err := hosts.Sync(ctx, db.DB, entries); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"Alice", "Bob", "Carol"} {
		id := int64(i + 1)
		mustExec(t, db, "INSERT INTO students(id, name, grp, created_at) VALUES(?, ?, 'A', 0)", id, name)
		mustExec(t, db, "INSERT INTO telegram_links(student_id, tg_user_id, linked_at) VALUES(?, ?, 0)", id, 1000+id)
	}
	engine := NewEngine(db, 3*time.Minute, 2*time.Minute)
	signer := token.NewSigner(bytes.Repeat([]byte{1}, 32))
	ci := &Checkin{Engine: engine, Signer: signer, Board: NewQRBoard(signer, 30*time.Second), Hosts: hosts, TTL: 60 * time.Second}
	return &env{t: t, ctx: ctx, db: db, engine: engine, checkin: ci, hosts: hosts}
}

func mustExec(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func idle(ms int64) sql.NullInt64 { return sql.NullInt64{Int64: ms, Valid: true} }

var noIdle = sql.NullInt64{}

func (e *env) agent(at time.Time, host, kind, sid string, idleMs sql.NullInt64) []Closure {
	e.t.Helper()
	cl, err := e.engine.AgentEvent(e.ctx, at, host, kind, sid, idleMs)
	if err != nil {
		e.t.Fatal(err)
	}
	return cl
}

func (e *env) start(at time.Time, host, sid string) { e.agent(at, host, KindSessionStart, sid, noIdle) }

// activeUntil sends a heartbeat every minute from `from` to `to` (inclusive)
// for a user who was last active at lastActive (or is active, if lastActive
// is after the heartbeat).
func (e *env) activeUntil(host, sid string, from, to, lastActive time.Time) {
	e.t.Helper()
	for t := from; !t.After(to); t = t.Add(time.Minute) {
		var ms int64
		if t.After(lastActive) {
			ms = t.Sub(lastActive).Milliseconds()
		}
		e.agent(t, host, KindHeartbeat, sid, idle(ms))
	}
}

func (e *env) hostID(name string) int64 {
	h, ok := e.hosts.ByName(name)
	if !ok {
		e.t.Fatalf("no host %s", name)
	}
	return h.ID
}

// scan shows a fresh code on host 5 s before `at` and scans it at `at`.
func (e *env) scan(at time.Time, host string, tgUser int64) ScanReply {
	e.t.Helper()
	tok, _ := e.checkin.Board.Current(e.hostID(host), at.Add(-5*time.Second))
	r, err := e.checkin.Scan(e.ctx, at, tgUser, tok)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) sweep(at time.Time) []Closure {
	e.t.Helper()
	cl, err := e.engine.Sweep(e.ctx, at)
	if err != nil {
		e.t.Fatal(err)
	}
	return cl
}

type visit struct {
	Student int64
	Host    string
	Start   time.Time
	End     time.Time // zero while open
	Reason  string
}

func (e *env) visits() []visit {
	e.t.Helper()
	rows, err := e.db.Query("SELECT student_id, host, confirmed_at, ended_at, COALESCE(end_reason, '') FROM presence ORDER BY id")
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []visit
	for rows.Next() {
		var v visit
		var s int64
		var end sql.NullInt64
		if err := rows.Scan(&v.Student, &v.Host, &s, &end, &v.Reason); err != nil {
			e.t.Fatal(err)
		}
		v.Start = store.FromMs(s)
		if end.Valid {
			v.End = store.FromMs(end.Int64)
		}
		out = append(out, v)
	}
	return out
}

func fmtVisits(vs []visit) string {
	var b bytes.Buffer
	for _, v := range vs {
		end := "open"
		if !v.End.IsZero() {
			end = v.End.In(almaty).Format("01-02 15:04:05")
		}
		fmt.Fprintf(&b, "\n  student %d %s %s -> %s %s", v.Student, v.Host, v.Start.In(almaty).Format("01-02 15:04:05"), end, v.Reason)
	}
	return b.String()
}

func (e *env) wantVisits(want ...visit) {
	e.t.Helper()
	got := e.visits()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(got); i++ {
		g, w := got[i], want[i]
		ok = g.Student == w.Student && g.Host == w.Host && g.Start.Equal(w.Start) && g.End.Equal(w.End) && g.Reason == w.Reason
	}
	if !ok {
		e.t.Fatalf("visits:%s\nwant:%s", fmtVisits(got), fmtVisits(want))
	}
}

// snapshot captures sessions and presence for replay comparison.
func (e *env) snapshot() string {
	e.t.Helper()
	var b bytes.Buffer
	for _, q := range []string{
		"SELECT id, host, sid, started_at, last_seen_at, last_activity_at, COALESCE(last_idle_ms, -1), implicit, COALESCE(ended_at, -1), COALESCE(end_reason, '') FROM sessions ORDER BY id",
		"SELECT id, student_id, host, session_id, confirmed_at, COALESCE(ended_at, -1), COALESCE(end_reason, ''), scan_latency_ms, stale_token FROM presence ORDER BY id",
	} {
		rows, err := e.db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
	}
	return b.String()
}
