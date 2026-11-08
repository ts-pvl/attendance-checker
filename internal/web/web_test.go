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
	pages := []string{"/admin/today", "/admin/hours", "/admin/hours.csv", "/admin/students", "/admin/students/slips", "/admin/anomalies", "/admin/audit"}
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
