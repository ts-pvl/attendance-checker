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
	"slices"
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
// for users, events, event members and XP transactions. It caps every page
// at two rows, as a server-side row limit would, and checks that nothing
// that varies is written into a query's text.
type fakePlatform struct {
	queries atomic.Int32
}

type fuser struct {
	id                 int64
	login              string
	first, last, email any // null or a string
	firstCyr, lastCyr  any
	firstLat, lastLat  any
	audit              any
}

var fakeUsers = []fuser{
	{101, "jdoe", "Ivan", "Petrov", "jdoe@example.org", "Иван", "Петров", "Ivan", "Petrov", 1.2},
	{102, "asmith", "Anna", "Smith", nil, nil, nil, nil, nil, nil},
	{103, "bkim", nil, nil, nil, nil, nil, nil, nil, 0.85},
	{104, "cpark", "Chan", nil, nil, "Чан", "Пак", nil, nil, nil}, // no full Latin name
	{105, "", "No", "Login", nil, nil, nil, nil, nil, nil},
}

type fevent struct {
	id, parent int64
	path       string
	start, end string
}

var fakeEvents = []fevent{
	{10, 0, "/campus/piscinego", "2026-03-02T05:00:00+00:00", "2026-03-27T13:00:00+00:00"},
	{11, 10, "/campus/piscinego/checkpoint-01", "2026-03-06T05:00:00+00:00", "2026-03-06T13:00:00+00:00"},
	{12, 0, "/campus/onboarding/check-in", "2026-02-27T05:00:00+00:00", "2026-02-27T13:00:00+00:00"},
	{20, 0, "/campus/module", "2025-10-01T05:00:00+00:00", ""},
}

type fmember struct{ id, user, event, level int64 }

var fakeMembers = []fmember{
	{1, 101, 12, 0}, {2, 101, 10, 7}, {3, 101, 11, 0}, {4, 102, 10, 3},
	{5, 103, 10, 0}, {6, 104, 20, 9}, {7, 104, 12, 0},
}

type ftx struct {
	id, user, event int64 // event 0: none (null)
	typ, amount, at string
}

var fakeTx = []ftx{
	{1, 101, 10, "xp", "500", "2026-03-03T10:00:00+00:00"},
	{2, 101, 10, "up", "999", "2026-03-03T11:00:00+00:00"}, // not XP
	{3, 101, 10, "xp", "250", "2026-03-06T12:00:00.123456+00:00"},
	{4, 102, 10, "xp", "1000.4", "2026-03-04T09:00:00+00:00"},
	{5, 102, 0, "xp", "7777", "2026-03-04T10:00:00+00:00"}, // in no event
	{6, 102, 10, "xp", "0.4", "2026-03-04T09:30:00+00:00"},
	{7, 102, 10, "xp", "-50", "2026-03-05T09:00:00+00:00"}, // XP can be taken back
	{8, 104, 20, "xp", "120000", "2026-01-15T08:00:00+00:00"},
	{9, 101, 11, "xp", "40", "2026-03-05T13:00:00+00:00"}, // XP under a sub-event
}

func jsonVal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *fakePlatform) handler(t *testing.T) http.Handler {
	paths := regexp.MustCompile(`attrs\(path: "([^"]*)"\)`)
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
			var body struct {
				Query     string
				Variables struct {
					IDs    []int64  `json:"ids"`
					Logins []string `json:"logins"`
					After  int64    `json:"after"`
					Limit  int      `json:"limit"`
				}
			}
			json.NewDecoder(r.Body).Decode(&body)
			q, v := body.Query, body.Variables
			if regexp.MustCompile(`[0-9]{2}|jdoe|cpark`).MatchString(q) {
				t.Errorf("data written into the query text: %s", q)
			}
			if n := strings.Count(q, "attrs"); n != len(paths.FindAllString(q, -1)) {
				t.Errorf("the whole of attrs (personal documents) asked for: %s", q)
			}
			for _, m := range paths.FindAllStringSubmatch(q, -1) {
				if !slices.Contains([]string{"firstNameCyr", "lastNameCyr", "firstNameLat", "lastNameLat"}, m[1]) {
					t.Errorf("attrs path %q asked for", m[1])
				}
			}
			page := func(rows []string) string { return `{"data": {"rows": [` + strings.Join(rows, ",") + `]}}` }
			limit := min(2, v.Limit)
			switch {
			case strings.Contains(q, "user(where: {login:"):
				var out []string
				for _, u := range fakeUsers {
					if slices.Contains(v.Logins, u.login) {
						out = append(out, fmt.Sprintf(`{"id": %d}`, u.id))
					}
				}
				fmt.Fprintf(w, `{"data": {"user": [%s]}}`, strings.Join(out, ","))
			case strings.Contains(q, "user(where: {id:"):
				var out []string
				for _, u := range fakeUsers {
					if slices.Contains(v.IDs, u.id) {
						out = append(out, fmt.Sprintf(`{"id": %d, "login": %q, "firstName": %s, "lastName": %s, "email": %s, "auditRatio": %s,
							"firstNameCyr": %s, "lastNameCyr": %s, "firstNameLat": %s, "lastNameLat": %s}`,
							u.id, u.login, jsonVal(u.first), jsonVal(u.last), jsonVal(u.email), jsonVal(u.audit),
							jsonVal(u.firstCyr), jsonVal(u.lastCyr), jsonVal(u.firstLat), jsonVal(u.lastLat)))
					}
				}
				fmt.Fprintf(w, `{"data": {"user": [%s]}}`, strings.Join(out, ","))
			case strings.Contains(q, "event(where: {id:"):
				var out []string
				for _, e := range fakeEvents {
					if !slices.Contains(v.IDs, e.id) {
						continue
					}
					if !strings.Contains(q, "path") {
						out = append(out, fmt.Sprintf(`{"id": %d}`, e.id))
						continue
					}
					parent, end := "null", "null"
					if e.parent != 0 {
						parent = strconv.FormatInt(e.parent, 10)
					}
					if e.end != "" {
						end = strconv.Quote(e.end)
					}
					out = append(out, fmt.Sprintf(`{"id": %d, "path": %q, "parentId": %s, "startAt": %q, "endAt": %s}`, e.id, e.path, parent, e.start, end))
				}
				fmt.Fprintf(w, `{"data": {"event": [%s]}}`, strings.Join(out, ","))
			case strings.Contains(q, "event_user(where: {eventId:"), strings.Contains(q, "event_user(where: {userId:"):
				byEvent := strings.Contains(q, "eventId: {_in")
				var rows []string
				for _, m := range fakeMembers {
					if m.id > v.After && len(rows) < limit && (byEvent && slices.Contains(v.IDs, m.event) || !byEvent && slices.Contains(v.IDs, m.user)) {
						rows = append(rows, fmt.Sprintf(`{"id": %d, "userId": %d, "eventId": %d, "level": %d}`, m.id, m.user, m.event, m.level))
					}
				}
				io.WriteString(w, page(rows))
			case strings.Contains(q, "transaction(where:"):
				if !strings.Contains(q, `type: {_eq: "xp"}`) {
					t.Errorf("transaction query without the xp filter: %s", q)
				}
				var rows []string
				for _, x := range fakeTx {
					if x.id > v.After && x.typ == "xp" && slices.Contains(v.IDs, x.user) && len(rows) < limit {
						ev := "null"
						if x.event != 0 {
							ev = strconv.FormatInt(x.event, 10)
						}
						rows = append(rows, fmt.Sprintf(`{"id": %d, "userId": %d, "eventId": %s, "amount": %s, "createdAt": %q}`, x.id, x.user, ev, x.amount, x.at))
					}
				}
				io.WriteString(w, page(rows))
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

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// By hand, for the logins JDoe (capitals), cpark and nobody1, and events 10
// and 99 (whose members are jdoe, asmith and bkim):
//   - jdoe (101): Иван Петров, also "Ivan Petrov"; audit 1.2. In 12 (top
//     level), 10 (level 7, XP 500 + 250 = 750; the "up" is not XP) and its
//     checkpoint 11 (XP 40, so it is kept). Current: the latest XP is in 10
//     (6 March 12:00 against 5 March 13:00).
//   - asmith (102): Anna Smith, no other name, no audit ratio. In 10: level
//     3, XP 1000.4 + 0.4 - 50 = 950.8 → 951 (rounded once, at the end); the
//     7777 in no event counts nowhere.
//   - bkim (103): no names; audit 0.85; in 10 at level 0 with no XP, kept as
//     a top-level event, and current as the only one.
//   - cpark (104): Чан Пак; no other name, as the Latin one lacks a last
//     name. In 20 (level 9, XP 120000 in January) and 12 (started later, no
//     XP): current 20, the latest XP.
//   - nobody1 is not on the platform; event 99 neither: one warning. User
//     105 has no login and is in nothing asked for.
func TestFetch(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewTLSServer(f.handler(t))
	defer srv.Close()
	c := &Client{Base: srv.URL, Token: goodToken, HTTP: srv.Client()}
	if _, _, err := c.Fetch(context.Background(), Request{}); !errors.Is(err, ErrNothingToRead) || f.queries.Load() != 0 {
		t.Errorf("nothing asked for: %v, %d queries", err, f.queries.Load())
	}
	d, warnings, err := c.Fetch(context.Background(), Request{Logins: []string{"JDoe", "cpark", "nobody1"}, Events: []int64{10, 99}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "event 99 was not found on the platform" {
		t.Errorf("warnings: %q", warnings)
	}
	want := []User{
		{ID: 101, Login: "jdoe", FirstName: "Иван", LastName: "Петров", OtherName: "Ivan Petrov", Email: "jdoe@example.org", AuditRatio: 1.2, HasAuditRatio: true,
			Events: []EventStat{{10, 7, 750, at("2026-03-06T12:00:00.123456Z")}, {11, 0, 40, at("2026-03-05T13:00:00Z")}, {12, 0, 0, time.Time{}}}, Current: 10},
		{ID: 102, Login: "asmith", FirstName: "Anna", LastName: "Smith",
			Events: []EventStat{{10, 3, 951, at("2026-03-05T09:00:00Z")}}, Current: 10},
		{ID: 103, Login: "bkim", AuditRatio: 0.85, HasAuditRatio: true, Events: []EventStat{{10, 0, 0, time.Time{}}}, Current: 10},
		{ID: 104, Login: "cpark", FirstName: "Чан", LastName: "Пак",
			Events: []EventStat{{12, 0, 0, time.Time{}}, {20, 9, 120000, at("2026-01-15T08:00:00Z")}}, Current: 20},
	}
	if fmt.Sprint(d.Users) != fmt.Sprint(want) {
		t.Errorf("users:\n%+v\nwant\n%+v", d.Users, want)
	}
	wantEvents := []Event{
		{10, "/campus/piscinego", 0, at("2026-03-02T05:00:00Z"), at("2026-03-27T13:00:00Z")},
		{11, "/campus/piscinego/checkpoint-01", 10, at("2026-03-06T05:00:00Z"), at("2026-03-06T13:00:00Z")},
		{12, "/campus/onboarding/check-in", 0, at("2026-02-27T05:00:00Z"), at("2026-02-27T13:00:00Z")},
		{20, "/campus/module", 0, at("2025-10-01T05:00:00Z"), time.Time{}},
	}
	if fmt.Sprint(d.Events) != fmt.Sprint(wantEvents) {
		t.Errorf("events:\n%+v\nwant\n%+v", d.Events, wantEvents)
	}
	// Queries, by hand: logins 1; event check 1; members of 10 and 99
	// (rows 2, 4, 5 in pages of two: [2 4] [5] []) 3; users 1; their
	// memberships (rows 1-7: [1 2] [3 4] [5 6] [7] []) 5; their XP (rows 1,
	// 3-9: [1 3] [4 5] [6 7] [8 9] []) 5; the events 1. 17 in all.
	if n := f.queries.Load(); n != 17 {
		t.Errorf("%d queries, want 17", n)
	}
}

// The access token goes in a URL: it must never show up in an error.
func TestTokenNeverInErrors(t *testing.T) {
	srv := httptest.NewTLSServer((&fakePlatform{}).handler(t))
	bad := "wrongtoken12345"
	c := &Client{Base: srv.URL, Token: bad, HTTP: srv.Client()}
	_, _, err := c.Fetch(context.Background(), Request{Events: []int64{10}})
	if err == nil || strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("rejected token: %v", err)
	}
	srv.Close() // now the connection fails; Go's error would print the URL
	c.Token = goodToken
	_, _, err = c.Fetch(context.Background(), Request{Logins: []string{"jdoe"}})
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
	if ev, err := ParseEvents(" 15, 12;12 7 "); err != nil || fmt.Sprint(ev) != "[7 12 15]" {
		t.Errorf("ParseEvents: %v %v", ev, err)
	}
	for _, bad := range []string{"12, x", "-3", "0", strings.Repeat("1 ", 0) + "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21"} {
		if _, err := ParseEvents(bad); err == nil {
			t.Errorf("ParseEvents(%q) accepted", bad)
		}
	}
	if s := (Settings{Events: []int64{7, 12}}); !s.Has(12) || s.Has(1) || s.EventsText() != "7, 12" {
		t.Errorf("settings: %+v", s)
	}
}

// By hand, for the students JDoe (jdoe in other case), nobody1 (not on the
// platform), one without a login and cpark (archived, so not read):
//   - logins only: jdoe alone; 1 user with XP (750 in 10); 1 linked, 1
//     unknown login, 1 without a login;
//   - with all of event 10: also asmith (951) and bkim (no XP): 3 users, 2
//     with XP, the same links.
func TestSyncer(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewTLSServer(f.handler(t))
	defer srv.Close()
	db := newDB(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	// The test server is https://127.0.0.1:port.
	s := &Syncer{DB: db, Base: srv.URL, TokenFile: filepath.Join(dir, "platform_token"), HTTP: srv.Client()}

	if _, err := (&Syncer{DB: db, TokenFile: s.TokenFile}).Sync(ctx, now, "boss"); !errors.Is(err, ErrNoPlatform) {
		t.Errorf("no platform_url: %v", err)
	}
	if _, err := s.Sync(ctx, now, "boss"); !errors.Is(err, ErrNoToken) {
		t.Errorf("no token: %v", err)
	}
	if s.Ready() || s.Due(ctx, now, time.Hour) {
		t.Error("ready without a token")
	}
	if err := s.SaveSettings(ctx, now, "10, x", "", "boss"); !errors.Is(err, ErrBadEvents) {
		t.Errorf("bad events accepted: %v", err)
	}
	if err := s.SaveSettings(ctx, now, "", "short", "boss"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token accepted: %v", err)
	}
	if err := s.SaveSettings(ctx, now, "", goodToken, "boss"); err != nil {
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
	if _, err := s.Sync(ctx, now, "boss"); !errors.Is(err, ErrNothingToRead) {
		t.Errorf("no logins, no events: %v", err)
	}

	mustExec(t, db, `INSERT INTO students(id, name, grp, login, archived, created_at) VALUES
		(1, 'Петров Иван', 'A', 'JDoe', 0, 0), (2, 'Ghost', 'A', 'nobody1', 0, 0), (3, 'Smith Anna', 'B', '', 0, 0), (4, 'Old', 'B', 'cpark', 1, 0)`)
	// The failed try above counts for the schedule.
	if !s.Ready() || s.Due(ctx, now, time.Hour) || !s.Due(ctx, now.Add(time.Hour), time.Hour) {
		t.Error("schedule after a failed try")
	}
	res, err := s.Sync(ctx, now, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if res.Users != 1 || res.WithXP != 1 || res.Linked != 1 || res.UnknownLogins != 1 || res.StudentsNoLogin != 1 {
		t.Errorf("logins only: %+v", res)
	}
	var level, xp, event int64
	var ratio float64
	var other string
	db.QueryRow("SELECT level, xp, event_id, audit_ratio, other_name FROM platform_users WHERE lower(login) = 'jdoe'").Scan(&level, &xp, &event, &ratio, &other)
	if level != 7 || xp != 750 || event != 10 || ratio != 1.2 || other != "Ivan Petrov" {
		t.Errorf("jdoe: level %d, xp %d, event %d, audit %v, %q", level, xp, event, ratio, other)
	}
	var rows string
	db.QueryRow("SELECT group_concat(event_id || ':' || level || ':' || xp, ' ') FROM (SELECT * FROM platform_user_events ORDER BY event_id)").Scan(&rows)
	if rows != "10:7:750 11:0:40 12:0:0" {
		t.Errorf("jdoe's events: %q", rows)
	}
	db.QueryRow("SELECT group_concat(id || ':' || COALESCE(parent_id, '-'), ' ') FROM (SELECT * FROM platform_events ORDER BY id)").Scan(&rows)
	if rows != "10:- 11:10 12:-" {
		t.Errorf("events: %q", rows)
	}

	if err := s.SetEvent(ctx, now, 10, true, "boss"); err != nil {
		t.Fatal(err)
	}
	if set, _ := LoadSettings(ctx, db); set.EventsText() != "10" {
		t.Errorf("events after SetEvent: %q", set.EventsText())
	}
	res, err = s.Sync(ctx, now, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if res.Users != 3 || res.WithXP != 2 || res.Linked != 1 || res.UnknownLogins != 1 || res.StudentsNoLogin != 1 || len(res.Warnings) != 0 {
		t.Errorf("with event 10: %+v", res)
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
	var stored int
	db.QueryRow("SELECT COUNT(*) FROM platform_users").Scan(&stored)
	if stored != 3 {
		t.Errorf("platform users after a failed sync: %d", stored)
	}

	if err := s.SetEvent(ctx, now, 10, false, "boss"); err != nil {
		t.Fatal(err)
	}
	if set, _ := LoadSettings(ctx, db); len(set.Events) != 0 {
		t.Errorf("events after taking 10 out: %v", set.Events)
	}
	if err := s.SetEvent(ctx, now, 0, true, "boss"); !errors.Is(err, ErrBadEvents) {
		t.Errorf("event 0: %v", err)
	}
}

// By hand, with platform users jdoe (Иван Петров), asmith (Anna Smith),
// bkim (no names), dlee (Дана Ли), ekim (a last name only) and gnur
// (Гульнара Нурланова, also "Gulnara Nurlanova"):
//   - "Петров Иван" and "Петров Иван Сергеевич" both match jdoe: ambiguous, neither is linked;
//   - "Smith Anna Maria" matches asmith only: linked;
//   - "Ким Борис": bkim has no names and ekim only a last name, so nothing;
//   - "Ли Дана" matches dlee, but dlee is already the login of another student: nothing;
//   - "Anna Smith" (archived) is not considered, so asmith stays unambiguous;
//   - "Nurlanova Gulnara", written in Latin letters, matches gnur's other name.
func TestLinkByName(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	if _, err := Store(ctx, db, now, Data{Users: []User{
		{ID: 1, Login: "jdoe", FirstName: "Иван", LastName: "Петров"},
		{ID: 2, Login: "asmith", FirstName: "Anna", LastName: "Smith"},
		{ID: 3, Login: "bkim"},
		{ID: 4, Login: "dlee", FirstName: "Дана", LastName: "Ли"},
		{ID: 5, Login: "ekim", LastName: "Ким"}, // a last name alone is not enough
		{ID: 6, Login: "gnur", FirstName: "Гульнара", LastName: "Нурланова", OtherName: "Gulnara Nurlanova"},
	}}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO students(id, name, login, archived, created_at) VALUES
		(1, 'Петров Иван', '', 0, 0), (2, 'Петров Иван Сергеевич', '', 0, 0), (3, 'Smith Anna Maria', '', 0, 0),
		(4, 'Ким Борис', '', 0, 0), (5, 'Ли Дана', '', 0, 0), (6, 'Dana Lee Old', 'dlee', 0, 0), (7, 'Anna Smith', '', 1, 0),
		(8, 'Nurlanova Gulnara', '', 0, 0)`)
	links, err := LinkByName(ctx, db, now, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(links) != "[{3 Smith Anna Maria asmith} {8 Nurlanova Gulnara gnur}]" {
		t.Errorf("links: %v", links)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'set_login' AND actor = 'boss'").Scan(&n)
	if n != 2 {
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
