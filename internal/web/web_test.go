package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"attendance/internal/config"
	"attendance/internal/core"
	"attendance/internal/hours"
	"attendance/internal/i18n"
	"attendance/internal/store"
	"attendance/internal/token"
)

const (
	hostKey  = "test-host-key-0123456789"
	pcIP     = "10.0.5.11"
	staffIP  = "10.0.9.50"
	outsider = "203.0.113.7"
)

type testServer struct {
	t   *testing.T
	s   *Server
	h   http.Handler
	now time.Time
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hostsFile := filepath.Join(dir, "hosts.csv")
	os.WriteFile(hostsFile, []byte("name,ip\nPC-01,"+pcIP+"\nPC-02,10.0.5.12\n"), 0o644)
	hosts := core.NewHosts(hostsFile)
	if err := hosts.Load(ctx, db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.BotUsername = "TestBot"
	cfg.Timezone, _ = time.LoadLocation("Asia/Almaty")
	cfg.Lang = "en"
	cfg.AdminNetworks = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8")}
	engine := core.NewEngine(db, cfg.HeartbeatTimeout, cfg.CheckoutSlack)
	signer := token.NewSigner(bytes.Repeat([]byte{3}, 32))
	board := core.NewQRBoard(signer, cfg.QRRotate)
	ts := &testServer{t: t, now: time.Date(2026, 3, 10, 10, 0, 0, 0, cfg.Timezone)}
	ts.s = &Server{
		Cfg: cfg, DB: db, Engine: engine, Hosts: hosts, Board: board,
		Checkin: &core.Checkin{Engine: engine, Signer: signer, Board: board, Hosts: hosts, TTL: cfg.QRTokenTTL},
		Calc:    hours.Calc{Loc: cfg.Timezone}, HostKey: hostKey,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return ts.now },
	}
	ts.h, err = ts.s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ name, role string }{{"boss", "admin"}, {"teach", "teacher"}} {
		if err := core.CreateStaff(ctx, db, ts.now, u.name, "correct horse battery", u.role); err != nil {
			t.Fatal(err)
		}
	}
	return ts
}

type req struct {
	method, path, ip string
	form             url.Values
	header           map[string]string
	cookie           *http.Cookie
}

func (ts *testServer) do(r req) *httptest.ResponseRecorder {
	ts.t.Helper()
	var body io.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	hr.RemoteAddr = r.ip + ":40000"
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	if r.cookie != nil {
		hr.AddCookie(r.cookie)
	}
	w := httptest.NewRecorder()
	ts.h.ServeHTTP(w, hr)
	return w
}

func (ts *testServer) event(ip, key, kind string) int {
	return ts.do(req{method: "POST", path: "/event", ip: ip,
		form:   url.Values{"kind": {kind}, "sid": {"boot-1"}, "idle_ms": {"1000"}},
		header: map[string]string{"X-Host-Key": key}}).Code
}

func (ts *testServer) login(user string) (*http.Cookie, string) {
	ts.t.Helper()
	w := ts.do(req{method: "POST", path: "/admin/login", ip: staffIP,
		form: url.Values{"username": {user}, "password": {"correct horse battery"}}})
	if w.Code != http.StatusSeeOther {
		ts.t.Fatalf("login %s: %d", user, w.Code)
	}
	var c *http.Cookie
	for _, x := range w.Result().Cookies() {
		if x.Name == sessionCookie {
			c = x
		}
	}
	page := ts.do(req{method: "GET", path: "/admin/students", ip: staffIP, cookie: c})
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if m == nil {
		ts.t.Fatalf("no csrf token on page")
	}
	return c, m[1]
}

func TestEventAuthentication(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct {
		name, ip, key string
		want          int
	}{
		{"known PC, right key", pcIP, hostKey, 200},
		{"known PC, forged key", pcIP, "guess", 401},
		{"known PC, no key", pcIP, "", 401},
		{"unlisted address, right key", "10.0.5.99", hostKey, 403},
		{"outside address", outsider, hostKey, 403},
	}
	for _, tc := range cases {
		if got := ts.event(tc.ip, tc.key, core.KindSessionStart); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := ts.event(pcIP, hostKey, "reboot_everything"); got != 400 {
		t.Errorf("unknown kind: %d", got)
	}
	// The host is identified by its address: one session on PC-01 only.
	var host string
	ts.s.DB.QueryRow("SELECT host FROM sessions").Scan(&host)
	if host != "PC-01" {
		t.Errorf("session host %q", host)
	}
}

func TestQRPage(t *testing.T) {
	ts := newTestServer(t)
	if w := ts.do(req{method: "GET", path: "/qr", ip: outsider}); w.Code != 403 {
		t.Errorf("/qr from outside: %d", w.Code)
	}
	if w := ts.do(req{method: "GET", path: "/qr/state", ip: staffIP}); w.Code != 403 {
		t.Errorf("/qr/state from a non-PC: %d", w.Code)
	}
	w := ts.do(req{method: "GET", path: "/qr/state", ip: pcIP})
	if !strings.Contains(w.Body.String(), `"state":"login"`) {
		t.Errorf("no session yet: %s", w.Body)
	}
	ts.event(pcIP, hostKey, core.KindSessionStart)
	w = ts.do(req{method: "GET", path: "/qr/state", ip: pcIP})
	if !strings.Contains(w.Body.String(), `"link":"https://t.me/TestBot?start=c`) {
		t.Errorf("QR state: %s", w.Body)
	}
	// After the timeout without heartbeats the page goes back to "log in first".
	ts.now = ts.now.Add(4 * time.Minute)
	w = ts.do(req{method: "GET", path: "/qr/state", ip: pcIP})
	if !strings.Contains(w.Body.String(), `"state":"login"`) {
		t.Errorf("stale session still shows a code: %s", w.Body)
	}
	if w := ts.do(req{method: "GET", path: "/qr", ip: pcIP}); w.Code != 200 || !strings.Contains(w.Body.String(), "PC-01") {
		t.Errorf("/qr page: %d", w.Code)
	}
}

func TestAdminAccess(t *testing.T) {
	ts := newTestServer(t)
	pages := []string{"/admin/today", "/admin/hours", "/admin/hours.csv", "/admin/students", "/admin/students/slips", "/admin/anomalies", "/admin/audit",
		"/admin/dashboard", "/admin/dashboard.csv"}
	for _, p := range pages {
		if w := ts.do(req{method: "GET", path: p, ip: staffIP}); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/login" {
			t.Errorf("%s without login: %d %s", p, w.Code, w.Header().Get("Location"))
		}
	}
	// Lab PCs and outside addresses cannot reach the admin UI at all.
	for _, ip := range []string{pcIP, outsider} {
		if w := ts.do(req{method: "GET", path: "/admin/login", ip: ip}); w.Code != 403 {
			t.Errorf("login page from %s: %d", ip, w.Code)
		}
	}
	admin, adminCSRF := ts.login("boss")
	for _, p := range pages {
		if w := ts.do(req{method: "GET", path: p, ip: staffIP, cookie: admin}); w.Code != 200 {
			t.Errorf("%s as admin: %d", p, w.Code)
		}
	}
	// A valid session cookie from a non-admin network is still refused.
	if w := ts.do(req{method: "GET", path: "/admin/today", ip: outsider, cookie: admin}); w.Code != 403 {
		t.Errorf("admin cookie from outside: %d", w.Code)
	}
	add := func(c *http.Cookie, csrf string) int {
		return ts.do(req{method: "POST", path: "/admin/students/add", ip: staffIP, cookie: c,
			form: url.Values{"name": {"Zed"}, "csrf": {csrf}}}).Code
	}
	if got := add(admin, ""); got != 403 {
		t.Errorf("POST without CSRF token: %d", got)
	}
	if got := add(admin, "wrong"); got != 403 {
		t.Errorf("POST with wrong CSRF token: %d", got)
	}
	if got := add(admin, adminCSRF); got != http.StatusSeeOther {
		t.Errorf("admin add student: %d", got)
	}
	teacher, teacherCSRF := ts.login("teach")
	if w := ts.do(req{method: "GET", path: "/admin/hours", ip: staffIP, cookie: teacher}); w.Code != 200 {
		t.Errorf("teacher reads hours: %d", w.Code)
	}
	if got := add(teacher, teacherCSRF); got != 403 {
		t.Errorf("teacher writes: %d", got)
	}
	if w := ts.do(req{method: "GET", path: "/admin/audit", ip: staffIP, cookie: teacher}); w.Code != 403 {
		t.Errorf("teacher reads audit log: %d", w.Code)
	}
	// Session idle timeout.
	ts.now = ts.now.Add(ts.s.Cfg.StaffSessionIdle + time.Minute)
	if w := ts.do(req{method: "GET", path: "/admin/today", ip: staffIP, cookie: admin}); w.Code != http.StatusSeeOther {
		t.Errorf("expired session: %d", w.Code)
	}
}

func TestLoginThrottling(t *testing.T) {
	ts := newTestServer(t)
	bad := func() int {
		return ts.do(req{method: "POST", path: "/admin/login", ip: staffIP,
			form: url.Values{"username": {"boss"}, "password": {"nope"}}}).Code
	}
	for i := 0; i < loginMaxFails; i++ {
		if got := bad(); got != 401 {
			t.Fatalf("attempt %d: %d", i, got)
		}
	}
	if got := bad(); got != 429 {
		t.Errorf("after %d failures: %d, want 429", loginMaxFails, got)
	}
	// Even the right password is refused while locked.
	w := ts.do(req{method: "POST", path: "/admin/login", ip: staffIP,
		form: url.Values{"username": {"boss"}, "password": {"correct horse battery"}}})
	if w.Code != 429 {
		t.Errorf("right password while locked: %d", w.Code)
	}
	ts.now = ts.now.Add(loginWindow + time.Second)
	if _, csrf := ts.login("boss"); csrf == "" {
		t.Error("login after lockout expired failed")
	}
	// Cross-site login posts are rejected.
	w = ts.do(req{method: "POST", path: "/admin/login", ip: staffIP, header: map[string]string{"Origin": "http://evil.example"},
		form: url.Values{"username": {"boss"}, "password": {"correct horse battery"}}})
	if w.Code != 403 {
		t.Errorf("cross-origin login: %d", w.Code)
	}
	// "Origin: null" (what browsers send under Referrer-Policy: no-referrer)
	// is refused, so the policy must let the real origin through.
	w = ts.do(req{method: "POST", path: "/admin/login", ip: staffIP, header: map[string]string{"Origin": "null"},
		form: url.Values{"username": {"boss"}, "password": {"correct horse battery"}}})
	if w.Code != 403 {
		t.Errorf("Origin null login: %d", w.Code)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
	// A browser on the same origin logs in.
	w = ts.do(req{method: "POST", path: "/admin/login", ip: staffIP, header: map[string]string{"Origin": "http://example.com"},
		form: url.Values{"username": {"boss"}, "password": {"correct horse battery"}}})
	if w.Code != 303 {
		t.Errorf("same-origin login: %d", w.Code)
	}
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	if w := ts.do(req{method: "GET", path: "/healthz", ip: staffIP}); w.Code != 403 {
		t.Errorf("/healthz from LAN: %d", w.Code)
	}
	if w := ts.do(req{method: "GET", path: "/healthz", ip: "127.0.0.1"}); w.Code != 200 {
		t.Errorf("/healthz: %d %s", w.Code, w.Body)
	}
	// The lab empties normally (logout): silence is fine.
	ts.event(pcIP, hostKey, core.KindSessionStart)
	ts.now = ts.now.Add(time.Minute)
	ts.event(pcIP, hostKey, core.KindLogout)
	ts.now = ts.now.Add(time.Hour)
	if w := ts.do(req{method: "GET", path: "/healthz", ip: "127.0.0.1"}); w.Code != 200 {
		t.Errorf("after a normal logout: %d %s", w.Code, w.Body)
	}
	// PCs stop reporting mid-session: alert after the timeout and 15 minutes.
	ts.event(pcIP, hostKey, core.KindSessionStart)
	ts.now = ts.now.Add(20 * time.Minute)
	ts.s.Engine.Sweep(context.Background(), ts.now)
	if w := ts.do(req{method: "GET", path: "/healthz", ip: "127.0.0.1"}); w.Code != 503 {
		t.Errorf("after a timeout: %d %s", w.Code, w.Body)
	}
}

// Every i18n key used in a template must exist.
func TestTemplateKeysExist(t *testing.T) {
	re := regexp.MustCompile(`\bt "([a-z_.0-9]+)"`)
	entries, _ := templateFS.ReadDir("templates")
	for _, e := range entries {
		b, _ := templateFS.ReadFile("templates/" + e.Name())
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if !i18n.Has(m[1]) {
				t.Errorf("%s: missing i18n key %q", e.Name(), m[1])
			}
		}
	}
	for _, r := range []string{core.ScanBadToken, core.ScanExpired, core.ScanUsed, core.ScanUnknownHost, core.ScanNotLinked, core.ScanNoSession} {
		if !i18n.Has("scan." + r) {
			t.Errorf("missing scan.%s", r)
		}
	}
	for _, r := range []string{core.ReasonIdleLogout, core.ReasonLogout, core.ReasonTimeout, core.ReasonReplaced, core.ReasonAutoClosed, core.ReasonCheckout, core.ReasonMoved, core.ReasonTakenOver, core.ReasonManual} {
		if !i18n.Has("reason." + r) {
			t.Errorf("missing reason.%s", r)
		}
	}
}

func TestPagesRenderWithData(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	ts.event(pcIP, hostKey, core.KindSessionStart)
	core.AddStudent(ctx, ts.s.DB, ts.now, "Alice", "A", "test")
	core.AddCorrection(ctx, ts.s.DB, ts.now, core.CorrectionInput{Action: "add", StudentID: 1, Host: "PC-01",
		Start: ts.now.Add(-2 * time.Hour), End: ts.now.Add(-time.Hour), Reason: "no phone", Actor: "boss"})
	core.GenerateCode(ctx, ts.s.DB, ts.now, 1, time.Hour, "boss")
	admin, _ := ts.login("boss")
	for _, p := range []string{"/admin/today", "/admin/today/live", "/admin/hours?period=day", "/admin/hours?period=week",
		"/admin/hours?period=month&empty=1", "/admin/students", "/admin/students/1", "/admin/students/slips",
		"/admin/anomalies", "/admin/audit", "/admin/hours.csv?period=week"} {
		w := ts.do(req{method: "GET", path: p, ip: staffIP, cookie: admin})
		if w.Code != 200 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	w := ts.do(req{method: "GET", path: "/admin/hours.csv?period=day", ip: staffIP, cookie: admin})
	if !strings.Contains(w.Body.String(), "Alice,A,day,2026-03-10,2026-03-10,60,1.00,1") {
		t.Errorf("CSV:\n%s", w.Body)
	}
}

func TestDashboard(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	core.AddStudent(ctx, ts.s.DB, ts.now, "Alice", "A", "test")
	// 08:00-09:00 today (ts.now is 10:00).
	core.AddCorrection(ctx, ts.s.DB, ts.now, core.CorrectionInput{Action: "add", StudentID: 1, Host: "PC-01",
		Start: ts.now.Add(-2 * time.Hour), End: ts.now.Add(-time.Hour), Reason: "no phone", Actor: "boss"})
	lang := ts.s.Cfg.Lang
	for _, user := range []string{"boss", "teach"} {
		c, _ := ts.login(user)
		w := ts.do(req{method: "GET", path: "/admin/dashboard", ip: staffIP, cookie: c})
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, i18n.T(lang, "dash.title")) || !strings.Contains(body, ">Alice</a>") ||
			!strings.Contains(body, i18n.T(lang, "dash.st_active")) {
			t.Fatalf("dashboard as %s: %d\n%s", user, w.Code, body)
		}
	}
	admin, _ := ts.login("boss")
	// One student, 1 h on the only school day: 100 %, arrival 08:00, active.
	w := ts.do(req{method: "GET", path: "/admin/dashboard.csv", ip: staffIP, cookie: admin})
	if !strings.Contains(w.Body.String(), "\n1,Alice,A,active,1.00,1,100.0,1.00,1,08:00,2026-03-10,") {
		t.Errorf("CSV:\n%s", w.Body)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attendance-2026-02-11-2026-03-10.csv") {
		t.Errorf("default period is the last 28 days: %s", cd)
	}
	w = ts.do(req{method: "GET", path: "/admin/dashboard?status=left", ip: staffIP, cookie: admin})
	if strings.Contains(w.Body.String(), ">Alice</a>") || !strings.Contains(w.Body.String(), i18n.T(lang, "dash.empty")) {
		t.Errorf("left filter still shows Alice")
	}
	// Bad, reversed and huge ranges are corrected rather than rejected.
	for _, q := range []string{"from=junk&to=2026-13-40", "from=2026-03-10&to=2026-01-01", "from=2000-01-01&to=2026-03-10"} {
		if w := ts.do(req{method: "GET", path: "/admin/dashboard?" + q, ip: staffIP, cookie: admin}); w.Code != 200 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	w = ts.do(req{method: "GET", path: "/admin/dashboard.csv?from=2000-01-01&to=2026-03-10", ip: staffIP, cookie: admin})
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attendance-2025-03-10-2026-03-10.csv") {
		t.Errorf("range is capped at 366 days: %s", cd)
	}
}

func TestUISwitch(t *testing.T) {
	ts := newTestServer(t)
	admin, _ := ts.login("boss")
	if w := ts.do(req{method: "GET", path: "/admin/today", ip: staffIP, cookie: admin}); !strings.Contains(w.Body.String(), `href="/admin/ui/new"`) {
		t.Error("old UI has no switch to the dashboard")
	}
	if w := ts.do(req{method: "GET", path: "/admin/dashboard", ip: staffIP, cookie: admin}); !strings.Contains(w.Body.String(), `href="/admin/ui/old"`) {
		t.Error("dashboard has no switch back")
	}
	w := ts.do(req{method: "GET", path: "/admin/ui/new", ip: staffIP, cookie: admin})
	var ui *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == uiCookie {
			ui = c
		}
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/dashboard" || ui == nil || ui.Value != "new" || !ui.HttpOnly {
		t.Fatalf("switch to new: %d %s %+v", w.Code, w.Header().Get("Location"), ui)
	}
	// The choice is remembered for /admin/ and the next login.
	w = ts.do(req{method: "GET", path: "/admin/", ip: staffIP, cookie: admin, header: map[string]string{"Cookie": "att_ui=new"}})
	if w.Header().Get("Location") != "/admin/dashboard" {
		t.Errorf("/admin/ after choosing the dashboard: %s", w.Header().Get("Location"))
	}
	w = ts.do(req{method: "POST", path: "/admin/login", ip: staffIP, header: map[string]string{"Cookie": "att_ui=new"},
		form: url.Values{"username": {"boss"}, "password": {"correct horse battery"}}})
	if w.Header().Get("Location") != "/admin/dashboard" {
		t.Errorf("login after choosing the dashboard: %s", w.Header().Get("Location"))
	}
	w = ts.do(req{method: "GET", path: "/admin/ui/old", ip: staffIP, cookie: admin})
	if w.Header().Get("Location") != "/admin/today" || !strings.Contains(w.Header().Get("Set-Cookie"), "att_ui=old") {
		t.Errorf("switch back: %s %s", w.Header().Get("Location"), w.Header().Get("Set-Cookie"))
	}
	// Without a choice the old UI stays the start page.
	if w := ts.do(req{method: "GET", path: "/admin/", ip: staffIP, cookie: admin}); w.Header().Get("Location") != "/admin/today" {
		t.Errorf("default start page: %s", w.Header().Get("Location"))
	}
}

// The students table of the dashboard is sorted, filtered and paged on the
// server; every state is a plain URL.
func TestDashboardTable(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	lang := ts.s.Cfg.Lang
	// Hours yesterday, 08:00 + h. Expected orders below are written by hand.
	for i, st := range []struct {
		name, group string
		h           int
	}{
		{"Борисов Антон", "A", 5}, {"Алиев Бек", "B", 3}, {"Валиева Гуля", "A", 1}, {"Ершов Дан", "B", 4}, {"Гаврилов Ерлан", "A", 2},
	} {
		core.AddStudent(ctx, ts.s.DB, ts.now, st.name, st.group, "test")
		start := ts.now.AddDate(0, 0, -1).Add(-2 * time.Hour)
		core.AddCorrection(ctx, ts.s.DB, ts.now, core.CorrectionInput{Action: "add", StudentID: int64(i + 1), Host: "PC-01",
			Start: start, End: start.Add(time.Duration(st.h) * time.Hour), Reason: "test", Actor: "boss"})
	}
	admin, _ := ts.login("boss")
	rowRE := regexp.MustCompile(`<tr data-name="([^"]+)"`)
	get := func(q string, hdr map[string]string) string {
		t.Helper()
		w := ts.do(req{method: "GET", path: "/admin/dashboard?" + q, ip: staffIP, cookie: admin, header: hdr})
		if w.Code != 200 {
			t.Fatalf("%s: %d", q, w.Code)
		}
		return w.Body.String()
	}
	rows := func(q string) string {
		var names []string
		for _, m := range rowRE.FindAllStringSubmatch(get(q, nil), -1) {
			names = append(names, strings.Fields(m[1])[0])
		}
		return strings.Join(names, ",")
	}
	cases := []struct{ q, want string }{
		{"", "Борисов,Ершов,Алиев,Гаврилов,Валиева"},                        // hours, most first
		{"per=2", "Борисов,Ершов"},                                          // first page
		{"per=2&page=2", "Алиев,Гаврилов"},                                  // second page
		{"per=2&page=99", "Валиева"},                                        // past the end: last page
		{"per=2&page=-3", "Борисов,Ершов"},                                  // nonsense: first page
		{"per=abc", "Борисов,Ершов,Алиев,Гаврилов,Валиева"},                 // nonsense: 30
		{"per=all", "Борисов,Ершов,Алиев,Гаврилов,Валиева"},                 // everything
		{"sort=name", "Алиев,Борисов,Валиева,Гаврилов,Ершов"},               // text: A→Z first
		{"sort=name&dir=desc", "Ершов,Гаврилов,Валиева,Борисов,Алиев"},      //
		{"sort=hours&dir=asc", "Валиева,Гаврилов,Алиев,Ершов,Борисов"},      //
		{"sort=group&dir=asc", "Борисов,Гаврилов,Валиева,Ершов,Алиев"},      // A then B, hours within
		{"sort=bogus&dir=sideways", "Борисов,Ершов,Алиев,Гаврилов,Валиева"}, // defaults
		{"group=A", "Борисов,Гаврилов,Валиева"},                             //
		{"group=B&sort=hours&dir=asc", "Алиев,Ершов"},                       //
		{"group=Z", ""},     //
		{"q=ВА", "Валиева"}, // case-insensitive name search
		{"q=Ёрш", "Ершов"},  // ё matches е
		{"q=ов&group=A&sort=name&per=1&page=2", "Гаврилов"}, // Борисов, Гаврилов match in A
	}
	for _, c := range cases {
		if got := rows(c.q); got != c.want {
			t.Errorf("%q: %s, want %s", c.q, got, c.want)
		}
	}
	body := get("per=2&page=2", nil)
	if !strings.Contains(body, i18n.T(lang, "dash.shown", 3, 4, 5)) {
		t.Error("no '3–4 of 5' on page 2")
	}
	// Every column header is a sort link that keeps the other settings.
	for _, key := range []string{"name", "group", "status", "hours", "days", "att", "perday", "sessions", "arrival", "last"} {
		if !strings.Contains(body, "sort="+key) {
			t.Errorf("no sort link for %s", key)
		}
	}
	if !strings.Contains(body, "per=2") || !strings.Contains(body, "page=3") {
		t.Error("pager links lose the page size or miss the next page")
	}
	// An htmx request for the table gets only the table.
	frag := get("per=2&page=2", map[string]string{"HX-Request": "true", "HX-Target": "students-table"})
	if strings.Contains(frag, "<html") || strings.Contains(frag, `class="kpis"`) || !strings.Contains(frag, `id="students-table"`) ||
		rowRE.FindAllString(frag, -1) == nil {
		t.Errorf("fragment:\n%s", frag)
	}
	// Restoring history after a reload needs the whole page.
	if full := get("per=2", map[string]string{"HX-Request": "true", "HX-Target": "students-table", "HX-History-Restore-Request": "true"}); !strings.Contains(full, "<html") {
		t.Error("history restore did not get the full page")
	}
	// CSV: same filter and order, all pages.
	w := ts.do(req{method: "GET", path: "/admin/dashboard.csv?group=A&sort=name&per=1&page=2", ip: staffIP, cookie: admin})
	var names []string
	for _, line := range strings.Split(w.Body.String(), "\n")[1:] {
		if f := strings.Split(line, ","); len(f) > 1 {
			names = append(names, strings.Fields(f[1])[0])
		}
	}
	if got := strings.Join(names, ","); got != "Борисов,Валиева,Гаврилов" {
		t.Errorf("CSV rows: %s", got)
	}
}

func TestCompareNames(t *testing.T) {
	// Russian order: case and ё do not matter; Cyrillic before Latin.
	sorted := []string{"Алиев Бек", "алиев Ерлан", "Ёлкина Анна", "Ершов Дан", "Яковлева Яна", "Adams Kate", "zed"}
	for i := 1; i < len(sorted); i++ {
		if compareNames(sorted[i-1], sorted[i]) >= 0 || compareNames(sorted[i], sorted[i-1]) <= 0 {
			t.Errorf("%q should come before %q", sorted[i-1], sorted[i])
		}
	}
	if compareNames("Ёлкин", "елкин") != 0 {
		t.Error("ё and е should compare equal")
	}
}
