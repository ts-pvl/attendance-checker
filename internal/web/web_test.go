package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// testLogOut: set to os.Stderr to see server errors while debugging.
var testLogOut io.Writer = io.Discard

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
	cfg.PlatformURL = "" // tests never reach the real platform
	cfg.AdminNetworks = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8")}
	engine := core.NewEngine(db, cfg.HeartbeatTimeout, cfg.CheckoutSlack)
	signer := token.NewSigner(bytes.Repeat([]byte{3}, 32))
	board := core.NewQRBoard(signer, cfg.QRRotate)
	ts := &testServer{t: t, now: time.Date(2026, 3, 10, 10, 0, 0, 0, cfg.Timezone)}
	ts.s = &Server{
		Cfg: cfg, DB: db, Engine: engine, Hosts: hosts, Board: board,
		Checkin: &core.Checkin{Engine: engine, Signer: signer, Board: board, Hosts: hosts, TTL: cfg.QRTokenTTL},
		Calc:    hours.Calc{Loc: cfg.Timezone}, HostKey: hostKey,
		Log: slog.New(slog.NewTextHandler(testLogOut, nil)), Now: func() time.Time { return ts.now },
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
	pages := []string{"/admin/today", "/admin/hours", "/admin/hours.csv", "/admin/students", "/admin/students.csv", "/admin/students/slips",
		"/admin/anomalies", "/admin/journal", "/admin/help", "/admin/platform", "/admin/dashboard", "/admin/dashboard.csv"}
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
	if w := ts.do(req{method: "GET", path: "/admin/journal", ip: staffIP, cookie: teacher}); w.Code != 403 {
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
	for _, p := range []string{"/admin/today", "/admin/hours?period=day", "/admin/hours?period=week",
		"/admin/hours?period=month&empty=1", "/admin/hours?period=week&per=all&page=3", "/admin/students", "/admin/students/1",
		"/admin/students/slips", "/admin/anomalies", "/admin/journal", "/admin/hours.csv?period=week"} {
		w := ts.do(req{method: "GET", path: p, ip: staffIP, cookie: admin})
		if w.Code != 200 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	w := ts.do(req{method: "GET", path: "/admin/hours.csv?period=day", ip: staffIP, cookie: admin})
	if !strings.Contains(w.Body.String(), "Alice,A,day,2026-03-10,2026-03-10,60,1.00,1") {
		t.Errorf("CSV:\n%s", w.Body)
	}
	lang := ts.s.Cfg.Lang
	// The student page's period arrows keep both parameters.
	body := ts.do(req{method: "GET", path: "/admin/students/1", ip: staffIP, cookie: admin}).Body.String()
	if !strings.Contains(body, `href="/admin/students/1?date=2026-02-01&amp;period=month"`) || !strings.Contains(body, `href="/admin/students/1?date=2026-04-01&amp;period=month"`) {
		t.Error("student page: month links")
	}
	// Today: the system status (no bot in the tests) above the PC grid.
	body = ts.do(req{method: "GET", path: "/admin/today", ip: staffIP, cookie: admin}).Body.String()
	for _, want := range []string{i18n.T(lang, "today.last_heartbeat"), i18n.T(lang, "today.last_scan"), i18n.T(lang, "today.bot_off"), i18n.T(lang, "today.open_visits")} {
		if !strings.Contains(body, want) {
			t.Errorf("Today lacks %q", want)
		}
	}
	// Hours: Alice's hour, the rules and the CSV link of the same period.
	body = ts.do(req{method: "GET", path: "/admin/hours?period=day", ip: staffIP, cookie: admin}).Body.String()
	for _, want := range []string{">Alice</a>", i18n.T(lang, "hours.policy_exact"), `href="/admin/hours.csv?date=2026-03-10&amp;period=day"`,
		`href="/admin/hours?date=2026-03-09&amp;period=day"`, `href="/admin/hours?date=2026-03-11&amp;period=day"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Hours lacks %q", want)
		}
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
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attendance-2026-03-09-2026-03-15.csv") {
		t.Errorf("default period is this week, Monday to Sunday: %s", cd)
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

// There is one admin UI: /admin/ and a fresh login go to the dashboard, and
// the addresses of the former second UI lead to the same pages.
func TestStartPageAndOldAddresses(t *testing.T) {
	ts := newTestServer(t)
	admin, _ := ts.login("boss")
	if w := ts.do(req{method: "GET", path: "/admin/", ip: staffIP, cookie: admin}); w.Header().Get("Location") != "/admin/dashboard" {
		t.Errorf("start page: %s", w.Header().Get("Location"))
	}
	w := ts.do(req{method: "POST", path: "/admin/login", ip: staffIP,
		form: url.Values{"username": {"boss"}, "password": {"correct horse battery"}}})
	if w.Header().Get("Location") != "/admin/dashboard" {
		t.Errorf("after login: %s", w.Header().Get("Location"))
	}
	if w := ts.do(req{method: "GET", path: "/", ip: staffIP}); w.Header().Get("Location") != "/admin/" {
		t.Errorf("/ from a staff PC: %s", w.Header().Get("Location"))
	}
	if w := ts.do(req{method: "GET", path: "/admin/dashboard", ip: staffIP, cookie: admin}); strings.Contains(w.Body.String(), "/admin/ui/") ||
		strings.Contains(w.Body.String(), "/admin/new/") {
		t.Error("the page still links to the old interface")
	}
	for from, to := range map[string]string{
		"/admin/new/students?group=A&page=2": "/admin/students?group=A&page=2",
		"/admin/new/students/7":              "/admin/students/7",
		"/admin/new/journal":                 "/admin/journal",
		"/admin/new/today":                   "/admin/today",
		"/admin/ui/old":                      "/admin/dashboard",
		"/admin/ui/new":                      "/admin/dashboard",
		"/admin/audit":                       "/admin/journal",
	} {
		w := ts.do(req{method: "GET", path: from, ip: staffIP, cookie: admin})
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != to {
			t.Errorf("%s: %d %s, want %s", from, w.Code, w.Header().Get("Location"), to)
		}
	}
	// Old addresses still need a login.
	if w := ts.do(req{method: "GET", path: "/admin/new/students", ip: staffIP}); w.Header().Get("Location") != "/admin/login" {
		t.Errorf("old address without login: %s", w.Header().Get("Location"))
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

func TestAdminTabs(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	core.AddStudent(ctx, ts.s.DB, ts.now, "Alice", "A", "test")
	pages := []string{"/admin/today", "/admin/hours", "/admin/help", "/admin/platform", "/admin/students", "/admin/students/1", "/admin/students/slips", "/admin/anomalies", "/admin/journal"}
	for _, p := range pages {
		if w := ts.do(req{method: "GET", path: p, ip: staffIP}); w.Code != http.StatusSeeOther {
			t.Errorf("%s without login: %d", p, w.Code)
		}
	}
	admin, _ := ts.login("boss")
	for _, p := range pages {
		w := ts.do(req{method: "GET", path: p, ip: staffIP, cookie: admin})
		if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/admin/hours"`) || !strings.Contains(w.Body.String(), `href="/admin/journal"`) {
			t.Errorf("%s as admin: %d, or a tab is missing", p, w.Code)
		}
	}
	if w := ts.do(req{method: "GET", path: "/admin/students/99", ip: staffIP, cookie: admin}); w.Code != 404 {
		t.Errorf("unknown student: %d", w.Code)
	}
	// Teachers read everything but the journal.
	teach, _ := ts.login("teach")
	for _, p := range pages {
		want := 200
		if p == "/admin/journal" {
			want = 403
		}
		if w := ts.do(req{method: "GET", path: p, ip: staffIP, cookie: teach}); w.Code != want {
			t.Errorf("%s as teacher: %d, want %d", p, w.Code, want)
		}
	}
	if w := ts.do(req{method: "GET", path: "/admin/students", ip: staffIP, cookie: teach}); strings.Contains(w.Body.String(), "/admin/students/import") ||
		strings.Contains(w.Body.String(), `href="/admin/journal"`) {
		t.Error("teacher sees admin actions or the journal tab")
	}
}

func TestStudentsList(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	for _, st := range []struct{ name, group string }{
		{"Борисов Антон", "B"}, {"Алиев Бек", "A"}, {"Валиева Гуля", "B"}, {"Ершов Дан", "A"}, {"Гаврилов Ерлан", "B"},
	} {
		core.AddStudent(ctx, ts.s.DB, ts.now, st.name, st.group, "test")
	}
	core.GenerateCode(ctx, ts.s.DB, ts.now, 3, time.Hour, "test") // Валиева: link sent
	core.SetArchived(ctx, ts.s.DB, ts.now, 5, true, "test")       // Гаврилов: archived
	admin, _ := ts.login("boss")
	nameRE := regexp.MustCompile(`<a class="name" href="/admin/students/\d+"[^>]*>([^<]+)</a>`)
	list := func(q string) string {
		t.Helper()
		w := ts.do(req{method: "GET", path: "/admin/students?" + q, ip: staffIP, cookie: admin})
		if w.Code != 200 {
			t.Fatalf("%s: %d", q, w.Code)
		}
		var names []string
		for _, m := range nameRE.FindAllStringSubmatch(w.Body.String(), -1) {
			names = append(names, strings.Fields(m[1])[0])
		}
		return strings.Join(names, ",")
	}
	// Expected orders by hand; archived Гаврилов only with archived=1.
	cases := []struct{ q, want string }{
		{"", "Алиев,Ершов,Борисов,Валиева"},                           // group, then name (as before)
		{"sort=name", "Алиев,Борисов,Валиева,Ершов"},                  //
		{"sort=name&dir=desc", "Ершов,Валиева,Борисов,Алиев"},         //
		{"sort=group&dir=desc", "Борисов,Валиева,Алиев,Ершов"},        // B then A, names A→Z within
		{"sort=status", "Валиева,Алиев,Ершов,Борисов"},                // link sent first, then the rest by group and name
		{"group=B", "Борисов,Валиева"},                                //
		{"archived=1", "Гаврилов"},                                    //
		{"per=3", "Алиев,Ершов,Борисов"},                              //
		{"per=3&page=2", "Валиева"},                                   //
		{"per=3&page=9", "Валиева"},                                   // past the end: last page
		{"per=all", "Алиев,Ершов,Борисов,Валиева"},                    //
		{"sort=nonsense&dir=sideways", "Алиев,Ершов,Борисов,Валиева"}, // defaults
	}
	for _, c := range cases {
		if got := list(c.q); got != c.want {
			t.Errorf("%q: %s, want %s", c.q, got, c.want)
		}
	}
	w := ts.do(req{method: "GET", path: "/admin/students?per=3&page=2", ip: staffIP, cookie: admin})
	body := w.Body.String()
	if !strings.Contains(body, i18n.T(ts.s.Cfg.Lang, "dash.shown", 4, 4, 4)) || !strings.Contains(body, "sort=status") {
		t.Error("pager text or status sort link missing")
	}
}

// The students list is searched by name or login and filtered by whether the
// student has linked Telegram; the CSV export has the same rows in the same
// order. All names, logins and Telegram users are invented.
func TestStudentsSearchAndExport(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	for _, st := range []struct{ name, group, login string }{
		{"Нұрланов Ерлан", "A", "nerlan"}, {"Smith Anna", "B", "asmith"}, {"Ёлкина Мария", "A", ""}, {"Smithson Bob", "A", "bsmith"},
	} {
		if _, err := core.AddStudentLogin(ctx, ts.s.DB, ts.now, st.name, st.group, st.login, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// Telegram: 1 with a username, 2 without one, 3 has an unused link, 4 nothing.
	for _, l := range []struct {
		id int64
		tg core.TGUser
	}{{1, core.TGUser{ID: 9001, Username: "erlan_n", Name: "Ерлан"}}, {2, core.TGUser{ID: 9002, Name: "Anna S"}}} {
		code, _ := core.GenerateCode(ctx, ts.s.DB, ts.now, l.id, time.Hour, "test")
		if r, err := core.Enroll(ctx, ts.s.DB, ts.now, l.tg, code); err != nil || r.Result != core.EnrollLinked {
			t.Fatalf("enroll %d: %v %+v", l.id, err, r)
		}
	}
	core.GenerateCode(ctx, ts.s.DB, ts.now, 3, time.Hour, "test")
	teach, _ := ts.login("teach") // read-only staff may search and export too
	nameRE := regexp.MustCompile(`<a class="name" href="/admin/students/\d+"[^>]*>([^<]+)</a>`)
	list := func(q string) string {
		t.Helper()
		w := ts.do(req{method: "GET", path: "/admin/students?" + q, ip: staffIP, cookie: teach})
		if w.Code != 200 {
			t.Fatalf("%s: %d", q, w.Code)
		}
		var names []string
		for _, m := range nameRE.FindAllStringSubmatch(w.Body.String(), -1) {
			names = append(names, m[1])
		}
		return strings.Join(names, ";")
	}
	// Default order: group, then name. A: Ёлкина, Нұрланов, Smithson (Cyrillic first); B: Smith.
	for q, want := range map[string]string{
		"":                             "Ёлкина Мария;Нұрланов Ерлан;Smithson Bob;Smith Anna",
		"q=smith":                      "Smithson Bob;Smith Anna",                             // part of a name, any case
		"q=SMITHSON":                   "Smithson Bob",                                        //
		"q=нурланов":                   "Нұрланов Ерлан",                                      // ұ typed as у
		"q=елкина":                     "Ёлкина Мария",                                        // ё typed as е
		"q=мария+ёлк":                  "Ёлкина Мария",                                        // every word, any order
		"q=мария+smith":                "",                                                    // no one has both
		"q=nerlan":                     "Нұрланов Ерлан",                                      // the platform login
		"q=bsm":                        "Smithson Bob",                                        // part of a login
		"q=++":                         "Ёлкина Мария;Нұрланов Ерлан;Smithson Bob;Smith Anna", // blank: everyone
		"tg=linked":                    "Нұрланов Ерлан;Smith Anna",                           // with or without a username
		"tg=unlinked":                  "Ёлкина Мария;Smithson Bob",                           // an unused link is not linked
		"tg=linked&q=smith":            "Smith Anna",                                          //
		"tg=unlinked&group=B":          "",                                                    //
		"tg=nonsense":                  "Ёлкина Мария;Нұрланов Ерлан;Smithson Bob;Smith Anna", // ignored
		"tg=linked&sort=name&dir=desc": "Smith Anna;Нұрланов Ерлан",                           // Latin after Cyrillic, reversed
	} {
		if got := list(q); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
	// The summary counts the whole group, before the search and the filter.
	body := ts.do(req{method: "GET", path: "/admin/students?q=smith&tg=linked", ip: staffIP, cookie: teach}).Body.String()
	if !strings.Contains(body, html.EscapeString(i18n.T(ts.s.Cfg.Lang, "students.summary", 4, 2, 1, 1))) {
		t.Error("summary should count all four students")
	}
	// Paging links and the export link keep the search and the filter.
	body = ts.do(req{method: "GET", path: "/admin/students?q=smith&per=1", ip: staffIP, cookie: teach}).Body.String()
	if !strings.Contains(body, `href="/admin/students?dir=asc&amp;page=2&amp;per=1&amp;q=smith&amp;sort=group"`) {
		t.Error("page 2 link lost the search")
	}
	if !strings.Contains(body, `href="/admin/students.csv?dir=asc&amp;q=smith&amp;sort=group"`) {
		t.Error("export link lost the search")
	}

	csvOf := func(q string) string {
		t.Helper()
		w := ts.do(req{method: "GET", path: "/admin/students.csv?" + q, ip: staffIP, cookie: teach})
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") ||
			w.Header().Get("Content-Disposition") != `attachment; filename="students-2026-03-10.csv"` {
			t.Fatalf("%s: %d %v", q, w.Code, w.Header())
		}
		return w.Body.String()
	}
	// Every row (no paging), in the list's order. The Telegram column: the
	// @username, or the display name for an account without one, or empty.
	want := "\ufeffname,group,login,telegram\n" +
		"Ёлкина Мария,A,,\n" +
		"Нұрланов Ерлан,A,nerlan,@erlan_n\n" +
		"Smithson Bob,A,bsmith,\n" +
		"Smith Anna,B,asmith,Anna S\n"
	if got := csvOf("per=1&page=2"); got != want {
		t.Errorf("CSV:\n%q\nwant\n%q", got, want)
	}
	if got := csvOf("q=smith&tg=linked"); got != "\ufeffname,group,login,telegram\nSmith Anna,B,asmith,Anna S\n" {
		t.Errorf("filtered CSV:\n%q", got)
	}
	if got := csvOf("sort=name&dir=desc&group=A"); got != "\ufeffname,group,login,telegram\nSmithson Bob,A,bsmith,\nНұрланов Ерлан,A,nerlan,@erlan_n\nЁлкина Мария,A,,\n" {
		t.Errorf("sorted CSV:\n%q", got)
	}
	// Archived students only with archived=1, as on the page.
	core.SetArchived(ctx, ts.s.DB, ts.now, 4, true, "test")
	if got := csvOf("q=smith"); got != "\ufeffname,group,login,telegram\nSmith Anna,B,asmith,Anna S\n" {
		t.Errorf("CSV with an archived student:\n%q", got)
	}
	if got := csvOf("q=smith&archived=1"); got != "\ufeffname,group,login,telegram\nSmithson Bob,A,bsmith,\n" {
		t.Errorf("archived CSV:\n%q", got)
	}
}

func TestReturnTo(t *testing.T) {
	cases := map[string]string{
		"/admin/students/3?period=month&date=2026-03-01": "/admin/students/3?period=month&date=2026-03-01",
		"":                            "/fallback",
		"https://evil.example/x":      "/fallback",
		"//evil.example/admin/":       "/fallback",
		"/qr":                         "/fallback",
		"/admin/x\r\nSet-Cookie: a=b": "/fallback",
		"/admin\\..\\x":               "/fallback",
		"javascript:alert(1)":         "/fallback",
	}
	for in, want := range cases {
		r := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{"return": {in}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := returnTo(r, "/fallback"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestActionsReturn(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	core.AddStudent(ctx, ts.s.DB, ts.now, "Alice", "A", "test")
	admin, csrf := ts.login("boss")
	post := func(path string, form url.Values) string {
		form.Set("csrf", csrf)
		w := ts.do(req{method: "POST", path: path, ip: staffIP, cookie: admin, form: form})
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Header().Get("Location")
	}
	if got := post("/admin/students/1/edit", url.Values{"name": {"Alice B"}, "return": {"/admin/students/1?period=week"}}); got != "/admin/students/1?period=week" {
		t.Errorf("edit went to %s", got)
	}
	if got := post("/admin/students/1/edit", url.Values{"name": {"Alice C"}, "return": {"https://evil.example/"}}); got != "/admin/students/1" {
		t.Errorf("edit with a foreign return went to %s", got)
	}
	if got := post("/admin/students/1/edit", url.Values{"name": {"Alice D"}}); got != "/admin/students/1" {
		t.Errorf("edit without a return went to %s", got)
	}
	if got := post("/admin/students/1/code", url.Values{}); got != "/admin/students/slips?id=1" {
		t.Errorf("new link went to %s", got)
	}
	if got := post("/admin/students/codes", url.Values{"group": {"A"}}); got != "/admin/students/slips?group=A" {
		t.Errorf("bulk links went to %s", got)
	}
}

func TestJournalPaging(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ { // five audit entries, "add_student" for Student1..Student5
		ts.now = ts.now.Add(time.Minute)
		core.AddStudent(ctx, ts.s.DB, ts.now, fmt.Sprintf("Student%d", i), "", "boss")
	}
	admin, _ := ts.login("boss") // the login is not audited
	targets := func(q string) string {
		w := ts.do(req{method: "GET", path: "/admin/journal?" + q, ip: staffIP, cookie: admin})
		var out []string
		for _, m := range regexp.MustCompile(`>student:(\d+)</a>`).FindAllStringSubmatch(w.Body.String(), -1) {
			out = append(out, m[1])
		}
		return strings.Join(out, ",")
	}
	// Newest first: student 5…1, then the two staff accounts the test server
	// created (no student link): 7 entries, pages of 2 → 4 pages.
	for q, want := range map[string]string{"": "5,4,3,2,1", "per=2": "5,4", "per=2&page=2": "3,2", "per=2&page=3": "1", "per=2&page=4": "", "per=all": "5,4,3,2,1"} {
		if got := targets(q); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
	// Past the end: the last page.
	w := ts.do(req{method: "GET", path: "/admin/journal?per=2&page=7", ip: staffIP, cookie: admin})
	if !strings.Contains(w.Body.String(), i18n.T(ts.s.Cfg.Lang, "dash.shown", 7, 7, 7)) {
		t.Error("page 7 of 4 should show the last page")
	}
}

// Anomalies through the real path: linked students, PC sessions, QR scans
// and agent pings. Expected lists by hand:
//   - Alice, PC-01, 21:30 → 03:40: long (6 h 09 min), active every minute,
//     scanned a 5 s old code that was still on screen → only in "long". The
//     time of day (21:31) does not matter.
//   - Bob, PC-01, 07:00 → 08:00: active only every 15 minutes → low activity.
//     He scanned a 40 s old code (under the 45 s limit) after the screen had
//     moved on to a newer one → old code. Two anomalies → "2–3 anomalies".
//   - Carol, PC-02, 20:00 → 22:00: scanned a 50 s old code (over the 45 s
//     limit) that was still the one on screen → only in "old code".
//
// Old-code list, newest check-in first: Bob (03-10 07:00), Carol (03-09 20:00).
func TestAnomalies(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	e := ts.s.Engine
	loc := ts.s.Cfg.Timezone
	at := func(day, hm string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, loc)
		return v
	}
	link := func(name string, tg int64) {
		id, _ := core.AddStudent(ctx, ts.s.DB, ts.now, name, "A", "test")
		code, _ := core.GenerateCode(ctx, ts.s.DB, ts.now, id, time.Hour, "test")
		core.Enroll(ctx, ts.s.DB, ts.now, core.TGUser{ID: tg}, code)
	}
	link("Alice", 501)
	link("Bob", 502)
	link("Carol", 503)
	pc, _ := ts.s.Hosts.ByName("PC-01")
	scan := func(tm time.Time, tg int64) {
		tok, _ := ts.s.Board.Current(pc.ID, tm.Add(-5*time.Second))
		if r, err := ts.s.Checkin.Scan(ctx, tm, tg, tok); err != nil || r.Result != core.ScanOK {
			t.Fatalf("scan: %v %+v", err, r)
		}
	}
	ms := func(d time.Duration) sql.NullInt64 { return sql.NullInt64{Int64: d.Milliseconds(), Valid: true} }
	// Alice
	start := at("2026-03-09", "21:30")
	e.AgentEvent(ctx, start, "PC-01", core.KindSessionStart, "s1", sql.NullInt64{})
	scan(start.Add(time.Minute), 501)
	end := at("2026-03-10", "03:40")
	for tm := start.Add(2 * time.Minute); tm.Before(end); tm = tm.Add(time.Minute) {
		e.AgentEvent(ctx, tm, "PC-01", core.KindHeartbeat, "s1", ms(5*time.Second))
	}
	e.AgentEvent(ctx, end, "PC-01", core.KindLogout, "s1", ms(0))
	// Bob: input at 07:15, 07:30, 07:45, 08:00 only.
	start = at("2026-03-10", "07:00")
	e.AgentEvent(ctx, start, "PC-01", core.KindSessionStart, "s2", sql.NullInt64{})
	oldTok, _ := ts.s.Board.Current(pc.ID, start.Add(-40*time.Second))
	ts.s.Board.Current(pc.ID, start.Add(-5*time.Second)) // 35 s later the screen shows a newer code
	if r, err := ts.s.Checkin.Scan(ctx, start, 502, oldTok); err != nil || r.Result != core.ScanOK {
		t.Fatalf("Bob's scan: %v %+v", err, r)
	}
	lastInput := start
	end = at("2026-03-10", "08:00")
	for tm := start.Add(time.Minute); tm.Before(end); tm = tm.Add(time.Minute) {
		if tm.Sub(start)%(15*time.Minute) == 0 {
			lastInput = tm
		}
		e.AgentEvent(ctx, tm, "PC-01", core.KindHeartbeat, "s2", ms(tm.Sub(lastInput)))
	}
	e.AgentEvent(ctx, end, "PC-01", core.KindLogout, "s2", ms(0))

	// Carol, PC-02, 20:00 → 22:00, active: not long, not low activity. Her
	// code is 50 s old and still the one on screen.
	pc2, _ := ts.s.Hosts.ByName("PC-02")
	start = at("2026-03-09", "20:00")
	e.AgentEvent(ctx, start, "PC-02", core.KindSessionStart, "s3", sql.NullInt64{})
	tok, _ := ts.s.Board.Current(pc2.ID, start.Add(-50*time.Second))
	if r, err := ts.s.Checkin.Scan(ctx, start, 503, tok); err != nil || r.Result != core.ScanOK {
		t.Fatalf("Carol's scan: %v %+v", err, r)
	}
	end = at("2026-03-09", "22:00")
	for tm := start.Add(time.Minute); tm.Before(end); tm = tm.Add(time.Minute) {
		e.AgentEvent(ctx, tm, "PC-02", core.KindHeartbeat, "s3", ms(time.Second))
	}
	e.AgentEvent(ctx, end, "PC-02", core.KindLogout, "s3", ms(0))

	r := httptest.NewRequest("GET", "/admin/anomalies?from=2026-03-09&to=2026-03-10", nil)
	d, err := ts.s.newAnomalies(r)
	if err != nil {
		t.Fatal(err)
	}
	names := func(l []anomVisit) string {
		var out []string
		for _, v := range l {
			out = append(out, v.Student.Name)
		}
		return strings.Join(out, ",")
	}
	if names(d.Low) != "Bob" || names(d.Long) != "Alice" || names(d.Old) != "Bob,Carol" || names(d.Multi) != "Bob" {
		t.Errorf("low %q long %q old %q multi %q", names(d.Low), names(d.Long), names(d.Old), names(d.Multi))
	}
	if len(d.Old) == 2 && (!d.Old[0].Stale || d.Old[0].LatencyMs != 40_000 || d.Old[1].Stale || d.Old[1].LatencyMs != 50_000) {
		t.Errorf("Bob: superseded %v, %d ms; Carol: superseded %v, %d ms; want true 40000, false 50000",
			d.Old[0].Stale, d.Old[0].LatencyMs, d.Old[1].Stale, d.Old[1].LatencyMs)
	}
	// Bob's moments of input: 07:00, 07:15, 07:30, 07:45 and the end, 08:00:
	// four 15-minute gaps. Active pings (idle under 90 s): 07:01, 07:15, 07:16,
	// 07:30, 07:31, 07:45, 07:46 = 7; 07:16 and 07:01 repeat a moment already counted.
	if len(d.Low) == 1 && (d.Low[0].Gap != 15*time.Minute || d.Low[0].Active != 7) {
		t.Errorf("Bob: median gap %v, active pings %d; want 15m, 7", d.Low[0].Gap, d.Low[0].Active)
	}
	if len(d.Multi) == 1 && strings.Join(d.Multi[0].Flags, ",") != i18n.T(ts.s.Cfg.Lang, "nanom.f_low")+","+i18n.T(ts.s.Cfg.Lang, "nanom.f_old") {
		t.Errorf("Bob's flags: %v", d.Multi[0].Flags)
	}
	admin, _ := ts.login("boss")
	if w := ts.do(req{method: "GET", path: "/admin/anomalies?from=2026-03-09&to=2026-03-10", ip: staffIP, cookie: admin}); w.Code != 200 ||
		!strings.Contains(w.Body.String(), ">Bob</a>") {
		t.Errorf("anomalies page: %d", w.Code)
	}
}

// The attendance matrix pages separately from the table; each one's links
// keep the other's settings.
func TestDashboardMatrixPaging(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	for i, st := range []struct {
		name string
		h    int
	}{{"Борисов", 5}, {"Алиев", 3}, {"Валиева", 1}, {"Ершов", 4}, {"Гаврилов", 2}} {
		core.AddStudent(ctx, ts.s.DB, ts.now, st.name, "A", "test")
		start := ts.now.AddDate(0, 0, -1).Add(-2 * time.Hour)
		core.AddCorrection(ctx, ts.s.DB, ts.now, core.CorrectionInput{Action: "add", StudentID: int64(i + 1), Host: "PC-01",
			Start: start, End: start.Add(time.Duration(st.h) * time.Hour), Reason: "test", Actor: "boss"})
	}
	admin, _ := ts.login("boss")
	matrixRE := regexp.MustCompile(`<text class="mn"[^>]*>([^<]+)</text>`)
	tableRE := regexp.MustCompile(`<tr data-name="([^"]+)"`)
	get := func(q string) (body, matrix, table string) {
		w := ts.do(req{method: "GET", path: "/admin/dashboard?" + q, ip: staffIP, cookie: admin})
		body = w.Body.String()
		var m, tb []string
		for _, x := range matrixRE.FindAllStringSubmatch(body, -1) {
			m = append(m, x[1])
		}
		for _, x := range tableRE.FindAllStringSubmatch(body, -1) {
			tb = append(tb, x[1])
		}
		return body, strings.Join(m, ","), strings.Join(tb, ",")
	}
	// Most hours first: Борисов 5, Ершов 4, Алиев 3, Гаврилов 2, Валиева 1.
	if _, m, _ := get(""); m != "Борисов,Ершов,Алиев,Гаврилов,Валиева" {
		t.Errorf("default (10 per page): %s", m)
	}
	if _, m, _ := get("mper=2&mpage=2"); m != "Алиев,Гаврилов" {
		t.Errorf("matrix page 2 of 2-row pages: %s", m)
	}
	if _, m, _ := get("mper=2&mpage=9"); m != "Валиева" {
		t.Errorf("matrix past the end: %s", m)
	}
	body, m, tb := get("per=2&page=2&mper=2&mpage=3")
	if m != "Валиева" || tb != "Алиев,Гаврилов" {
		t.Errorf("both paged: matrix %s, table %s", m, tb)
	}
	// The matrix's page links keep the table's page, and the other way round.
	if !strings.Contains(body, "mpage=2&amp;mper=2&amp;page=2&amp;per=2") && !strings.Contains(body, "mper=2&amp;mpage=2") {
		t.Error("matrix links lose the matrix settings")
	}
	for _, link := range regexp.MustCompile(`href="(/admin/dashboard\?[^"]*)"`).FindAllStringSubmatch(body, -1) {
		u, _ := url.Parse(strings.ReplaceAll(link[1], "&amp;", "&"))
		q := u.Query()
		if q.Get("mper") != "2" || q.Get("per") != "2" {
			t.Errorf("link drops a page size: %s", link[1])
		}
	}
	// An htmx request for either list gets both lists and nothing else.
	w := ts.do(req{method: "GET", path: "/admin/dashboard?mper=2", ip: staffIP, cookie: admin,
		header: map[string]string{"HX-Request": "true", "HX-Target": "matrix-block"}})
	frag := w.Body.String()
	if strings.Contains(frag, "<html") || !strings.Contains(frag, `id="matrix-block"`) || !strings.Contains(frag, `id="students-table"`) {
		t.Errorf("fragment for the matrix:\n%.300s", frag)
	}
}

// Default dates: the page shows this week (Monday–Sunday), the attendance
// matrix this month; the matrix's range is its own. ts.now is Tuesday
// 2026-03-10, so the week is 03-09…03-15 and the month 03-01…03-31.
func TestDashboardDateRanges(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	add := func(name string, day time.Time, h int) {
		id, _ := core.AddStudent(ctx, ts.s.DB, ts.now, name, "A", "test")
		start := day.Add(9 * time.Hour)
		core.AddCorrection(ctx, ts.s.DB, ts.now, core.CorrectionInput{Action: "add", StudentID: id, Host: "PC-01",
			Start: start, End: start.Add(time.Duration(h) * time.Hour), Reason: "test", Actor: "boss"})
	}
	loc := ts.s.Cfg.Timezone
	day := func(s string) time.Time { d, _ := time.ParseInLocation("2006-01-02", s, loc); return d }
	add("Неделин", day("2026-03-09"), 2)  // this week (and this month)
	add("Марсов", day("2026-03-02"), 3)   // this month, last week
	add("Февралев", day("2026-02-20"), 4) // February only
	admin, _ := ts.login("boss")
	page := func(q string) (body string, matrix, table []string) {
		t.Helper()
		w := ts.do(req{method: "GET", path: "/admin/dashboard?" + q, ip: staffIP, cookie: admin})
		if w.Code != 200 {
			t.Fatalf("%s: %d", q, w.Code)
		}
		body = w.Body.String()
		for _, m := range regexp.MustCompile(`<text class="mn"[^>]*>([^<]+)</text>`).FindAllStringSubmatch(body, -1) {
			matrix = append(matrix, m[1])
		}
		for _, m := range regexp.MustCompile(`<tr data-name="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			table = append(table, m[1])
		}
		return
	}
	days := func(body string) int {
		return len(regexp.MustCompile(`<text class="mh(?: we)?" `).FindAllString(body, -1))
	}
	input := func(body, name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return m[1]
	}

	body, matrix, table := page("")
	if input(body, "from") != "2026-03-09" || input(body, "to") != "2026-03-15" {
		t.Errorf("page dates %s–%s, want this week 2026-03-09–2026-03-15", input(body, "from"), input(body, "to"))
	}
	if input(body, "mfrom") != "2026-03-01" || input(body, "mto") != "2026-03-31" || days(body) != 31 {
		t.Errorf("matrix %s–%s with %d days, want March: 31 days", input(body, "mfrom"), input(body, "mto"), days(body))
	}
	if strings.Join(table, ",") != "Неделин" || strings.Join(matrix, ",") != "Марсов,Неделин" {
		t.Errorf("default: table %v (want this week's), matrix %v (want this month's, most hours first)", table, matrix)
	}

	// The matrix's own range changes only the matrix…
	body, matrix, table = page("mfrom=2026-02-01&mto=2026-02-28")
	if strings.Join(matrix, ",") != "Февралев" || days(body) != 28 || strings.Join(table, ",") != "Неделин" {
		t.Errorf("matrix in February: matrix %v (%d days), table %v", matrix, days(body), table)
	}
	// …and the page's range changes everything but the matrix.
	body, matrix, table = page("from=2026-03-02&to=2026-03-08")
	if strings.Join(table, ",") != "Марсов" || strings.Join(matrix, ",") != "Марсов,Неделин" || days(body) != 31 {
		t.Errorf("last week: table %v, matrix %v (%d days)", table, matrix, days(body))
	}
	// Reversed dates are swapped.
	if _, matrix, _ = page("mfrom=2026-02-28&mto=2026-02-01"); strings.Join(matrix, ",") != "Февралев" {
		t.Errorf("reversed matrix dates: %v", matrix)
	}
	// Links keep both ranges: the table's links carry the matrix's dates and
	// the matrix's links the page's dates.
	body, _, _ = page("from=2026-03-02&to=2026-03-08&mfrom=2026-02-01&mto=2026-02-28&per=1&mper=1")
	for _, link := range regexp.MustCompile(`href="(/admin/dashboard\?[^"]*)"`).FindAllStringSubmatch(body, -1) {
		u, _ := url.Parse(strings.ReplaceAll(link[1], "&amp;", "&"))
		q := u.Query()
		if q.Get("mfrom") != "2026-02-01" || q.Get("mto") != "2026-02-28" || q.Get("from") != "2026-03-02" {
			t.Errorf("link loses a date range: %s", link[1])
		}
	}
}

// The lists about check-ins and PC sessions, through the real path. On
// 2026-03-10, PCs heartbeat every minute from their login to 09:00:
//
//	08:00 PC-01 login, 08:01 Dave scans on PC-01
//	08:05 PC-02 login, 08:06 Dave scans on PC-02 (5 min later, PC-01 still
//	      in use until 09:00) → one student on several PCs
//	08:20 Eve scans on PC-01 → Dave and Eve in one PC-01 session
//	08:30 a Telegram account that is not linked scans on PC-01 → failed scan
//	09:01 the nightly cleanup closes Eve (PC-01) and Dave (PC-02) at 09:00
//	      → auto-closed, newest check-in first: Eve, Dave
func TestScanAnomalies(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	e := ts.s.Engine
	at := func(hm string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", "2026-03-10 "+hm, ts.s.Cfg.Timezone)
		return v
	}
	for i, name := range []string{"Dave", "Eve"} {
		id, _ := core.AddStudent(ctx, ts.s.DB, ts.now, name, "A", "test")
		code, _ := core.GenerateCode(ctx, ts.s.DB, ts.now, id, time.Hour, "test")
		core.Enroll(ctx, ts.s.DB, ts.now, core.TGUser{ID: int64(504 + i)}, code)
	}
	type step struct {
		at time.Time
		do func()
	}
	var steps []step
	idle := sql.NullInt64{Int64: 0, Valid: true}
	pcSession := func(pc, sid, from string) {
		start := at(from)
		steps = append(steps, step{start, func() { e.AgentEvent(ctx, start, pc, core.KindSessionStart, sid, sql.NullInt64{}) }})
		for tm := start.Add(time.Minute); !tm.After(at("09:00")); tm = tm.Add(time.Minute) {
			steps = append(steps, step{tm, func() { e.AgentEvent(ctx, tm, pc, core.KindHeartbeat, sid, idle) }})
		}
	}
	scan := func(hm, pc string, tg int64, want string) {
		tm := at(hm)
		steps = append(steps, step{tm.Add(time.Second), func() {
			h, _ := ts.s.Hosts.ByName(pc)
			tok, _ := ts.s.Board.Current(h.ID, tm.Add(-5*time.Second))
			if r, err := ts.s.Checkin.Scan(ctx, tm.Add(time.Second), tg, tok); err != nil || r.Result != want {
				t.Fatalf("scan at %s on %s: %v %+v", hm, pc, err, r)
			}
		}})
	}
	pcSession("PC-01", "a", "08:00")
	pcSession("PC-02", "b", "08:05")
	scan("08:01", "PC-01", 504, core.ScanOK)
	scan("08:06", "PC-02", 504, core.ScanOK)
	scan("08:20", "PC-01", 505, core.ScanOK)
	scan("08:30", "PC-01", 999, core.ScanNotLinked)
	steps = append(steps, step{at("09:01"), func() { e.Cleanup(ctx, at("09:01")) }})
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at.Before(steps[j].at) })
	for _, st := range steps {
		st.do()
	}

	d, err := ts.s.scanAnomalies(ctx, at("00:00"), at("00:00").AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Multi) != 1 || d.Multi[0].Student.Name != "Dave" || d.Multi[0].FirstHost != "PC-01" || d.Multi[0].SecondHost != "PC-02" ||
		!d.Multi[0].FirstActiveUntil.Equal(at("09:00")) {
		t.Errorf("several PCs: %+v", d.Multi)
	}
	if len(d.Many) != 1 || d.Many[0].Host != "PC-01" || !d.Many[0].Started.Equal(at("08:00")) || len(d.Many[0].Students) != 2 {
		t.Errorf("several students: %+v", d.Many)
	} else if got := d.Many[0].Students; !(got[0] == "Dave" && got[1] == "Eve" || got[0] == "Eve" && got[1] == "Dave") {
		t.Errorf("several students: %v", got)
	}
	if len(d.Auto) != 2 || d.Auto[0].Student.Name != "Eve" || d.Auto[1].Student.Name != "Dave" || !d.Auto[0].End.Equal(at("09:00")) {
		t.Errorf("auto-closed: %+v", d.Auto)
	}
	if len(d.Failed) != 1 || d.Failed[0].TGUser != 999 || d.Failed[0].Result != core.ScanNotLinked || d.Failed[0].Host != "PC-01" {
		t.Errorf("failed scans: %+v", d.Failed)
	}
	// Another day: nothing.
	if d, _ := ts.s.scanAnomalies(ctx, at("00:00").AddDate(0, 0, 1), at("00:00").AddDate(0, 0, 2)); len(d.Multi)+len(d.Many)+len(d.Auto)+len(d.Failed) != 0 {
		t.Errorf("next day: %+v", d)
	}
	// The Anomalies tab shows all four lists.
	admin, _ := ts.login("boss")
	body := ts.do(req{method: "GET", path: "/admin/anomalies?from=2026-03-10&to=2026-03-10", ip: staffIP, cookie: admin}).Body.String()
	lang := ts.s.Cfg.Lang
	for _, want := range []string{i18n.T(lang, "anom.multi_pc"), i18n.T(lang, "anom.many"), i18n.T(lang, "anom.auto"), i18n.T(lang, "anom.failed"),
		"ID 999", html.EscapeString(i18n.T(lang, "scan.not_linked")), "PC-01 2026-03-10 08:01"} {
		if !strings.Contains(body, want) {
			t.Errorf("anomalies page lacks %q", want)
		}
	}
}

func TestAnomaliesDefaultToThisWeek(t *testing.T) {
	ts := newTestServer(t)
	d, err := ts.s.newAnomalies(httptest.NewRequest("GET", "/admin/anomalies", nil))
	if err != nil {
		t.Fatal(err)
	}
	if d.From != "2026-03-09" || d.To != "2026-03-15" {
		t.Errorf("default %s–%s, want this week 2026-03-09–2026-03-15", d.From, d.To)
	}
}

// The students-per-day chart joins its points with straight lines and draws
// every point. By hand, for 0, 2 and 4 students on a 380 x 270 chart: the
// plot runs from x 38 to 372 and y 244 (zero) to 12; the scale tops out at 4.
// Three slots of 111.33 put the points at x 93.7, 205.0 and 316.3, and the
// values at y 244, 128 and 12.
func TestLineChartStraight(t *testing.T) {
	c := lineChart("en", 380, 270, []float64{0, 2, 4}, []string{"a", "b", "c"}, []string{"0", "2", "4"})
	if want := "M93.7,244.0 L205.0,128.0 L316.3,12.0"; c.Line != want {
		t.Errorf("line %q, want %q", c.Line, want)
	}
	if want := "M93.7,244.0 L205.0,128.0 L316.3,12.0 L316.3,244.0 L93.7,244.0 Z"; c.Area != want {
		t.Errorf("area %q, want %q", c.Area, want)
	}
	if len(c.Dots) != 3 || c.Dots[1].X < 204.9 || c.Dots[1].X > 205.1 || c.Dots[1].Y != 128 || c.Dots[2].Title != "4" {
		t.Errorf("dots %+v", c.Dots)
	}
	// One point: a line of one move, and still one visible dot.
	if c := lineChart("en", 380, 270, []float64{3}, []string{"a"}, []string{"3"}); c.Line != "M205.0,12.0" || len(c.Dots) != 1 {
		t.Errorf("one point: %q, %d dots", c.Line, len(c.Dots))
	}

	// On the page, each point is a visible circle as well as a hover target.
	ts := newTestServer(t)
	core.AddStudent(context.Background(), ts.s.DB, ts.now, "Alice", "A", "test")
	admin, _ := ts.login("boss")
	body := ts.do(req{method: "GET", path: "/admin/dashboard?from=2026-03-02&to=2026-03-08", ip: staffIP, cookie: admin}).Body.String()
	if pts, hits := strings.Count(body, `<circle class="pt" `), strings.Count(body, `<circle class="hit" `); pts != 7 || hits != 7 {
		t.Errorf("a week: %d visible points and %d hover targets, want 7 and 7", pts, hits)
	}
	if strings.Contains(body, `<path class="line" d="M`) && regexp.MustCompile(`<path class="line" d="[^"]*C`).MatchString(body) {
		t.Error("the line still has curves")
	}
}
