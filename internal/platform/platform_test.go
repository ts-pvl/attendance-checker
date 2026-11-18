package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"attendance/internal/store"
)

// All people and logins below are invented.
const (
	goodToken = "0a1b2c3d4e5f60718293a4b5c6d7e8f9fa0b1c2d"
	testJWT   = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
)

// fakePlatform answers like the 01Edu API: the token exchange, and GraphQL
// for events, XP transactions and users. It caps every page of transactions
// at two rows, as a server-side row limit would.
type fakePlatform struct {
	queries     atomic.Int32
	failUsers   bool // the audit ratio query fails
	badEventIDs bool
}

type ftx struct {
	id, user, event int64
	typ             string
	amount          string
}

var fakeTx = []ftx{
	{1, 101, 10, "xp", "500"},
	{2, 101, 10, "up", "999"}, // not XP
	{3, 101, 11, "xp", "250"},
	{4, 102, 10, "xp", "1000.4"}, // rounds to 1000
	{5, 102, 99, "xp", "7777"},   // another event
	{6, 104, 10, "xp", "300"},    // a user not registered in the events: ignored
	{7, 102, 10, "xp", "-50"},    // XP can be taken back
}

func (f *fakePlatform) handler(t *testing.T) http.Handler {
	in := regexp.MustCompile(`_in: \[([0-9, ]*)\]`)
	gt := regexp.MustCompile(`id: \{_gt: (\d+)\}`)
	ids := func(q string, n int) map[int64]bool {
		m := in.FindAllStringSubmatch(q, -1)
		out := map[int64]bool{}
		for _, s := range strings.Split(m[n][1], ",") {
			v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			out[v] = true
		}
		return out
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/token":
			if r.URL.Query().Get("token") != goodToken {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"Response code 401 (Unauthorized)"}`)
				return
			}
			json.NewEncoder(w).Encode(testJWT)
		case "/api/graphql-engine/v1/graphql":
			f.queries.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+testJWT {
				io.WriteString(w, `{"errors":[{"message":"Could not verify JWT"}]}`)
				return
			}
			var body struct{ Query string }
			json.NewDecoder(r.Body).Decode(&body)
			q := body.Query
			switch {
			case strings.Contains(q, "event(where"):
				ev := ids(q, 0)
				var out []string
				if ev[10] {
					out = append(out, `{"id": 10, "registrations": [{"users": [
						{"id": 101, "login": "jdoe", "attrs": {"firstNameCyr": "Иван", "lastNameCyr": "Петров", "email": "jdoe@example.org", "IIN": "000000000000", "phone": "+70000000000"}},
						{"id": 102, "login": "asmith", "attrs": {"firstName": "Anna", "lastName": "Smith"}}]}]}`)
				}
				if ev[11] {
					out = append(out, `{"id": 11, "registrations": [{"users": [
						{"id": 101, "login": "jdoe", "attrs": {"firstNameCyr": "Иван", "lastNameCyr": "Петров"}},
						{"id": 103, "login": "bkim", "attrs": null},
						{"id": 105, "login": "", "attrs": {}}]}]}`)
				}
				fmt.Fprintf(w, `{"data": {"event": [%s]}}`, strings.Join(out, ","))
			case strings.Contains(q, "transaction(where"):
				ev := ids(q, 0)
				after, _ := strconv.ParseInt(gt.FindStringSubmatch(q)[1], 10, 64)
				if !strings.Contains(q, `type: {_eq: "xp"}`) {
					t.Errorf("transaction query without the xp filter: %s", q)
				}
				var rows []string
				for _, x := range fakeTx {
					if x.id > after && x.typ == "xp" && ev[x.event] && len(rows) < 2 {
						rows = append(rows, fmt.Sprintf(`{"id": %d, "userId": %d, "amount": %s}`, x.id, x.user, x.amount))
					}
				}
				fmt.Fprintf(w, `{"data": {"transaction": [%s]}}`, strings.Join(rows, ","))
			case strings.Contains(q, "user(where"):
				if f.failUsers {
					io.WriteString(w, `{"errors":[{"message":"field 'auditRatio' not found in type: 'user'"}]}`)
					return
				}
				io.WriteString(w, `{"data": {"user": [{"id": 101, "auditRatio": 1.2}, {"id": 102, "auditRatio": null}, {"id": 103, "auditRatio": 0.85}]}}`)
			default:
				t.Errorf("unexpected query: %s", q)
			}
		default:
			w.WriteHeader(404)
		}
	})
}

func newDB(t *testing.T) *store.DB {
	db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// By hand, for events 10 and 11:
//   - jdoe (101): Петров Иван, in 10 and 11; XP 500 + 250 = 750 (the "up" is not XP); audit 1.2.
//   - asmith (102): Anna Smith, in 10; XP 1000.4 → 1000, then -50 = 950; event 99 does not count; no audit ratio.
//   - bkim (103): no names, in 11; XP 0; audit 0.85.
//   - a user without a login is skipped; user 104 is in no event, so his XP is ignored.
func TestFetch(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewTLSServer(f.handler(t))
	defer srv.Close()
	c := &Client{Base: srv.URL, Token: goodToken, HTTP: srv.Client()}
	users, warnings, err := c.Fetch(context.Background(), []int64{10, 11, 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "event 12 was not found on the platform" {
		t.Errorf("warnings: %q", warnings)
	}
	want := []User{
		{ID: 101, Login: "jdoe", FirstName: "Иван", LastName: "Петров", Email: "jdoe@example.org", Events: []int64{10, 11}, XP: 750, AuditRatio: 1.2, HasAuditRatio: true},
		{ID: 102, Login: "asmith", FirstName: "Anna", LastName: "Smith", Events: []int64{10}, XP: 950},
		{ID: 103, Login: "bkim", Events: []int64{11}, AuditRatio: 0.85, HasAuditRatio: true},
	}
	if fmt.Sprint(users) != fmt.Sprint(want) {
		t.Errorf("users:\n%+v\nwant\n%+v", users, want)
	}
	// 1 event query; XP rows in events 10-12 are ids 1, 3, 4, 6 (user 104,
	// returned and ignored) and 7, in pages of at most 2: [1 3] [4 6] [7] [] = 4;
	// 1 user query. 6 in all.
	if n := f.queries.Load(); n != 6 {
		t.Errorf("%d queries, want 6", n)
	}
	// Only the audit ratio fails: a warning, the rest is kept.
	f.failUsers = true
	users, warnings, err = c.Fetch(context.Background(), []int64{10})
	if err != nil || len(users) != 2 || users[0].HasAuditRatio || len(warnings) != 1 || !strings.Contains(warnings[0], "audit ratio not read") {
		t.Errorf("failing audit ratio: %v %+v %q", err, users, warnings)
	}
}

// The access token goes in a URL: it must never show up in an error.
func TestTokenNeverInErrors(t *testing.T) {
	srv := httptest.NewTLSServer((&fakePlatform{}).handler(t))
	bad := "wrongtoken12345"
	c := &Client{Base: srv.URL, Token: bad, HTTP: srv.Client()}
	_, _, err := c.Fetch(context.Background(), []int64{10})
	if err == nil || strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("rejected token: %v", err)
	}
	srv.Close() // now the connection fails; Go's error would print the URL
	c.Token = goodToken
	_, _, err = c.Fetch(context.Background(), []int64{10})
	if err == nil || strings.Contains(err.Error(), goodToken) || strings.Contains(err.Error(), "token=") {
		t.Errorf("connection error: %v", err)
	}
	// A server that echoes the token back.
	echo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "bad token "+r.URL.Query().Get("token"))
	}))
	defer echo.Close()
	c = &Client{Base: echo.URL, Token: goodToken, HTTP: echo.Client()}
	if _, err := c.JWT(context.Background()); err == nil || strings.Contains(err.Error(), goodToken) || !strings.Contains(err.Error(), "***") {
		t.Errorf("echoed token: %v", err)
	}
}

func TestSettings(t *testing.T) {
	for in, want := range map[string]string{"https://01.Example.org/": "https://01.example.org", "01.example.org": "https://01.example.org",
		"https://01.example.org/intra/x?y": "https://01.example.org", "": ""} {
		if got, err := CleanURL(in); err != nil || got != want {
			t.Errorf("CleanURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://01.example.org", "ftp://x", "https://user:pw@x.org", "https://"} {
		if _, err := CleanURL(bad); !errors.Is(err, ErrBadURL) {
			t.Errorf("CleanURL(%q) accepted", bad)
		}
	}
	if ev, err := ParseEvents(" 15, 12;12 7 "); err != nil || fmt.Sprint(ev) != "[7 12 15]" {
		t.Errorf("ParseEvents: %v %v", ev, err)
	}
	for _, bad := range []string{"12, x", "-3", "0"} {
		if _, err := ParseEvents(bad); err == nil {
			t.Errorf("ParseEvents(%q) accepted", bad)
		}
	}
}

func TestSyncer(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewTLSServer(f.handler(t))
	defer srv.Close()
	db := newDB(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s := &Syncer{DB: db, TokenFile: filepath.Join(dir, "platform_token"), HTTP: srv.Client()}

	if _, err := s.Sync(ctx, now, "boss"); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("not set up: %v", err)
	}
	if err := s.SaveSettings(ctx, now, "http://01.example.org", "10", "", "boss"); !errors.Is(err, ErrBadURL) {
		t.Errorf("http address accepted: %v", err)
	}
	if err := s.SaveSettings(ctx, now, srv.URL, "10, x", "", "boss"); !errors.Is(err, ErrBadEvents) {
		t.Errorf("bad events accepted: %v", err)
	}
	// The test server is https://127.0.0.1:port.
	if err := s.SaveSettings(ctx, now, srv.URL+"/", "11, 10", "", "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, now, "boss"); !errors.Is(err, ErrNoToken) {
		t.Errorf("no token: %v", err)
	}
	if err := s.SaveSettings(ctx, now, srv.URL, "10, 11", "short", "boss"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token accepted: %v", err)
	}
	if err := s.SaveSettings(ctx, now, srv.URL, "10, 11", goodToken, "boss"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.TokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", fi.Mode(), err)
	}
	var journal string
	db.QueryRow("SELECT details FROM audit_log WHERE action = 'platform_settings' ORDER BY id DESC").Scan(&journal)
	if strings.Contains(journal, goodToken) || !strings.Contains(journal, "new access token") {
		t.Errorf("journal: %q", journal)
	}

	// Students: one with jdoe's login in other case, one with a login the
	// platform does not have, one without a login, one archived.
	mustExec(t, db, `INSERT INTO students(id, name, grp, login, archived, created_at) VALUES
		(1, 'Петров Иван', 'A', 'JDoe', 0, 0), (2, 'Ghost', 'A', 'nobody1', 0, 0), (3, 'Smith Anna', 'B', '', 0, 0), (4, 'Old', 'B', 'bkim', 1, 0)`)
	if !s.Due(ctx, now, time.Hour) {
		t.Error("a first sync is due")
	}
	res, err := s.Sync(ctx, now, "boss")
	if err != nil {
		t.Fatal(err)
	}
	// 3 users, 2 with XP; linked: student 1; unknown login: student 2; no login: student 3 (4 is archived).
	if res.Users != 3 || res.WithXP != 2 || res.Linked != 1 || res.UnknownLogins != 1 || res.StudentsNoLogin != 1 {
		t.Errorf("result: %+v", res)
	}
	var xp int64
	var ratio float64
	db.QueryRow("SELECT xp, audit_ratio FROM platform_users WHERE lower(login) = 'jdoe'").Scan(&xp, &ratio)
	if xp != 750 || ratio != 1.2 {
		t.Errorf("jdoe: %d %v", xp, ratio)
	}
	var stored int
	var all string
	db.QueryRow("SELECT group_concat(first_name || last_name || email || events, '|') FROM platform_users").Scan(&all)
	if strings.Contains(all, "000000000000") || strings.Contains(all, "+7") {
		t.Error("ID numbers or phone numbers were stored")
	}
	st, _ := LoadStatus(ctx, db)
	if !st.LastSync.Equal(now) || st.LastError != "" || st.Result.Users != 3 {
		t.Errorf("status: %+v", st)
	}
	if s.Due(ctx, now.Add(59*time.Minute), time.Hour) || !s.Due(ctx, now.Add(time.Hour), time.Hour) || s.Due(ctx, now.Add(time.Hour), 0) {
		t.Error("schedule")
	}

	// A failure keeps the last good data and records a redacted error.
	if err := s.SaveToken("revokedtoken99"); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Hour)
	if _, err := s.Sync(ctx, later, ""); err == nil {
		t.Fatal("revoked token synced")
	}
	st, _ = LoadStatus(ctx, db)
	if !st.LastSync.Equal(now) || !st.LastTry.Equal(later) || !strings.Contains(st.LastError, "HTTP 401") || strings.Contains(st.LastError, "revokedtoken99") {
		t.Errorf("status after a failure: %+v", st)
	}
	db.QueryRow("SELECT COUNT(*) FROM platform_users").Scan(&stored)
	if stored != 3 {
		t.Errorf("platform users after a failed sync: %d", stored)
	}
}

// By hand, with platform users jdoe (Иван Петров), asmith (Anna Smith),
// bkim (no names) and dlee (Дана Ли):
//   - "Петров Иван" and "Петров Иван Сергеевич" both match jdoe: ambiguous, neither is linked;
//   - "Smith Anna Maria" matches asmith only: linked;
//   - "Ким Борис": bkim has no names and ekim only a last name, so nothing;
//   - "Ли Дана" matches dlee, but dlee is already the login of another student: nothing;
//   - "Anna Smith" (archived) is not considered, so asmith stays unambiguous.
func TestLinkByName(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	if _, err := Store(ctx, db, now, []User{
		{ID: 1, Login: "jdoe", FirstName: "Иван", LastName: "Петров"},
		{ID: 2, Login: "asmith", FirstName: "Anna", LastName: "Smith"},
		{ID: 3, Login: "bkim"},
		{ID: 4, Login: "dlee", FirstName: "Дана", LastName: "Ли"},
		{ID: 5, Login: "ekim", LastName: "Ким"}, // a last name alone is not enough
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO students(id, name, login, archived, created_at) VALUES
		(1, 'Петров Иван', '', 0, 0), (2, 'Петров Иван Сергеевич', '', 0, 0), (3, 'Smith Anna Maria', '', 0, 0),
		(4, 'Ким Борис', '', 0, 0), (5, 'Ли Дана', '', 0, 0), (6, 'Dana Lee Old', 'dlee', 0, 0), (7, 'Anna Smith', '', 1, 0)`)
	links, err := LinkByName(ctx, db, now, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(links) != "[{3 Smith Anna Maria asmith}]" {
		t.Errorf("links: %v", links)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'set_login' AND actor = 'boss'").Scan(&n)
	if n != 1 {
		t.Errorf("journal: %d", n)
	}
	// Run again: nothing new.
	if links, _ := LinkByName(ctx, db, now, "boss"); len(links) != 0 {
		t.Errorf("second run: %v", links)
	}
}

func mustExec(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
