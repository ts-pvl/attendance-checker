package bot

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"attendance/internal/core"
	"attendance/internal/hours"
	"attendance/internal/store"
	"attendance/internal/token"
)

type fakeTG struct {
	mu   sync.Mutex
	sent map[int64][]string
}

func (f *fakeTG) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeTG) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[chatID] = append(f.sent[chatID], text)
	return nil
}

func (f *fakeTG) last(chat int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.sent[chat]
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

type botEnv struct {
	t   *testing.T
	b   *Bot
	tg  *fakeTG
	ci  *core.Checkin
	db  *store.DB
	now time.Time
	upd int64
}

func newBotEnv(t *testing.T) *botEnv {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hosts := core.NewHosts("")
	hosts.Sync(ctx, db.DB, []core.HostEntry{{Name: "PC-17", IP: netip.MustParseAddr("10.0.0.17")}})
	engine := core.NewEngine(db, 3*time.Minute, 2*time.Minute)
	signer := token.NewSigner(bytes.Repeat([]byte{5}, 32))
	ci := &core.Checkin{Engine: engine, Signer: signer, Board: core.NewQRBoard(signer, 30*time.Second), Hosts: hosts, TTL: time.Minute}
	loc, _ := time.LoadLocation("Asia/Almaty")
	tg := &fakeTG{sent: map[int64][]string{}}
	e := &botEnv{t: t, tg: tg, ci: ci, db: db, now: time.Date(2026, 3, 10, 14, 0, 0, 0, loc)}
	e.b = New(tg, db, ci, hours.Calc{Loc: loc}, "en", slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.b.Now = func() time.Time { return e.now }
	db.Exec("INSERT INTO students(id, name, grp, created_at) VALUES(1, 'Alice Smith', 'A', 0)")
	return e
}

func (e *botEnv) send(user int64, chatType, text string) string {
	e.upd++
	e.b.HandleUpdate(context.Background(), Update{UpdateID: e.upd, Message: &Message{
		From: &User{ID: user, FirstName: "A"}, Chat: Chat{ID: user, Type: chatType}, Text: text,
	}})
	return e.tg.last(user)
}

func TestBotFlow(t *testing.T) {
	e := newBotEnv(t)
	ctx := context.Background()
	const alice = 777

	if got := e.send(alice, "private", "/start"); !strings.Contains(got, "personal link") {
		t.Errorf("welcome: %q", got)
	}
	code, err := core.GenerateCode(ctx, e.db, e.now, 1, time.Hour, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.send(alice, "private", "/start "+code); !strings.HasPrefix(got, "Linked as Alice Smith") {
		t.Errorf("enroll: %q", got)
	}
	if got := e.send(alice, "private", "/start "+code); !strings.Contains(got, "already linked as Alice Smith") {
		t.Errorf("enroll again: %q", got)
	}
	tok, _ := e.ci.Board.Current(1, e.now)
	if got := e.send(alice, "private", "/start "+tok); got != "Log in on the PC first." {
		t.Errorf("scan without session: %q", got)
	}
	e.ci.Engine.AgentEvent(ctx, e.now, "PC-17", core.KindSessionStart, "s", sql.NullInt64{})
	tok, _ = e.ci.Board.Current(1, e.now)
	if got := e.send(alice, "private", "/start "+tok); got != "Checked in at PC-17 ✅" {
		t.Errorf("scan: %q", got)
	}
	if got := e.send(alice, "private", "/start "+tok); !strings.Contains(got, "already used") {
		t.Errorf("replayed scan: %q", got)
	}
	e.now = e.now.Add(90 * time.Second)
	e.ci.Engine.AgentEvent(ctx, e.now, "PC-17", core.KindHeartbeat, "s", sql.NullInt64{Int64: 0, Valid: true})
	if got := e.send(alice, "private", "/status"); got != "Checked in at PC-17 since 14:00. Today: 1 min." {
		t.Errorf("status: %q", got)
	}
	if got := e.send(alice, "private", "/checkout"); got != "Checked out at 14:01. Today: 1 min." {
		t.Errorf("checkout: %q", got)
	}
	if got := e.send(alice, "private", "/checkout"); got != "You have no open visit." {
		t.Errorf("second checkout: %q", got)
	}
	tok, _ = e.ci.Board.Current(1, e.now)
	if got := e.send(4242, "private", "/start "+tok); got != "Your account isn't linked, ask staff." {
		t.Errorf("stranger: %q", got)
	}
	if got := e.send(alice, "private", "/start garbage"); !strings.Contains(got, "not valid") {
		t.Errorf("garbage: %q", got)
	}
}

func TestBotIgnoresGroups(t *testing.T) {
	e := newBotEnv(t)
	for _, typ := range []string{"group", "supergroup", "channel"} {
		if got := e.send(1, typ, "/status"); got != "" {
			t.Errorf("%s: replied %q", typ, got)
		}
	}
}

func TestBotRateLimit(t *testing.T) {
	e := newBotEnv(t)
	for i := 0; i < 10; i++ {
		e.send(1, "private", "/help")
	}
	if got := e.send(1, "private", "/help"); !strings.Contains(got, "Too many") {
		t.Errorf("11th message: %q", got)
	}
	n := len(e.tg.sent[1])
	e.send(1, "private", "/help")
	if len(e.tg.sent[1]) != n {
		t.Error("warned twice within a minute")
	}
	e.now = e.now.Add(30 * time.Second) // 10 tokens refill at one per 3 s
	if got := e.send(1, "private", "/help"); !strings.Contains(got, "/status") {
		t.Errorf("after refill: %q", got)
	}
}

func TestNotifyOnIdleLogout(t *testing.T) {
	e := newBotEnv(t)
	ctx := context.Background()
	e.db.Exec("INSERT INTO telegram_links(student_id, tg_user_id, linked_at) VALUES(1, 777, 0)")
	e.ci.Engine.AgentEvent(ctx, e.now, "PC-17", core.KindSessionStart, "s", sql.NullInt64{})
	tok, _ := e.ci.Board.Current(1, e.now)
	e.send(777, "private", "/start "+tok)
	e.now = e.now.Add(time.Minute)
	cl, _ := e.ci.Engine.AgentEvent(ctx, e.now, "PC-17", core.KindIdleLogout, "s", sql.NullInt64{Int64: 30_000, Valid: true})
	e.b.Notify(ctx, cl)
	select {
	case m := <-e.b.queue:
		if m.chatID != 777 || m.text != "Checked out at 14:00 (PC-17, idle logout). Today: 0 min." {
			t.Errorf("notification: %+v", m)
		}
	default:
		t.Fatal("no notification queued")
	}
}

func TestParseCommand(t *testing.T) {
	cases := map[string][2]string{
		"/start abc":           {"/start", "abc"},
		"/start@SchoolBot abc": {"/start", "abc"},
		"  /STATUS ":           {"/status", ""},
		"hello":                {"", "hello"},
	}
	for in, want := range cases {
		if c, a := parseCommand(in); c != want[0] || a != want[1] {
			t.Errorf("%q -> %q %q", in, c, a)
		}
	}
}
