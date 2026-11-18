package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"attendance/internal/core"
)

// A small fake platform (invented people): event 10 has jdoe (Иван Петров,
// 300 + 200 XP, audit 1.1) and asmith (Anna Smith, 50 XP).
func fakePlatform(t *testing.T, token string) *httptest.Server {
	const jwt = "aaa.bbb.ccc"
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/token":
			if r.URL.Query().Get("token") != token {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(jwt)
		case "/api/graphql-engine/v1/graphql":
			if r.Header.Get("Authorization") != "Bearer "+jwt {
				w.WriteHeader(401)
				return
			}
			b, _ := io.ReadAll(r.Body)
			q := string(b)
			switch {
			case strings.Contains(q, "event(where"):
				io.WriteString(w, `{"data":{"event":[{"id":10,"registrations":[{"users":[
					{"id":1,"login":"jdoe","attrs":{"firstNameCyr":"Иван","lastNameCyr":"Петров","email":"jdoe@example.org"}},
					{"id":2,"login":"asmith","attrs":{"firstName":"Anna","lastName":"Smith"}}]}]}]}}`)
			case strings.Contains(q, "_gt: 0"):
				io.WriteString(w, `{"data":{"transaction":[{"id":1,"userId":1,"amount":300},{"id":2,"userId":1,"amount":200},{"id":3,"userId":2,"amount":50}]}}`)
			case strings.Contains(q, "transaction(where"):
				io.WriteString(w, `{"data":{"transaction":[]}}`)
			case strings.Contains(q, "user(where"):
				io.WriteString(w, `{"data":{"user":[{"id":1,"auditRatio":1.1}]}}`)
			}
		}
	}))
}

func TestPlatformWeb(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	const token = "f00dbabe1234567890"
	srv := fakePlatform(t, token)
	defer srv.Close()
	ts.s.Cfg.PlatformTokenFile = filepath.Join(t.TempDir(), "platform_token")
	ts.s.syncer().HTTP = srv.Client()
	core.AddStudent(ctx, ts.s.DB, ts.now, "Alice", "A", "test")      // 1
	core.AddStudent(ctx, ts.s.DB, ts.now, "Smith Anna", "B", "test") // 2
	core.AddStudent(ctx, ts.s.DB, ts.now, "Bob", "A", "test")        // 3
	admin, csrf := ts.login("boss")
	teach, teachCSRF := ts.login("teach")
	post := func(c *http.Cookie, tok, path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", tok)
		return ts.do(req{method: "POST", path: path, ip: staffIP, cookie: c, form: form})
	}
	get := func(c *http.Cookie, path string) string {
		w := ts.do(req{method: "GET", path: path, ip: staffIP, cookie: c})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}

	if b := get(admin, "/admin/new/platform"); !strings.Contains(b, "The platform address and events are not set yet.") || !strings.Contains(b, `action="/admin/platform/settings"`) {
		t.Error("empty platform page")
	}
	if b := get(teach, "/admin/new/platform"); strings.Contains(b, "/admin/platform/") {
		t.Error("teacher sees platform actions")
	}
	for _, p := range []string{"/admin/platform/settings", "/admin/platform/sync", "/admin/platform/link"} {
		if w := post(teach, teachCSRF, p, url.Values{"url": {srv.URL}}); w.Code != http.StatusForbidden {
			t.Errorf("teacher posts %s: %d", p, w.Code)
		}
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/settings", url.Values{"url": {"http://x.org"}, "events": {"10"}})); !isErr || !strings.Contains(msg, "https://01.example.org") {
		t.Errorf("http address: %q", msg)
	}
	if msg, _ := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); msg != "The platform address and events are not set yet." {
		t.Errorf("sync before setup: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/settings", url.Values{"url": {srv.URL}, "events": {"10"}, "token": {token}})); isErr || msg != "Platform settings saved." {
		t.Errorf("save: %q", msg)
	}

	// Logins: Alice gets JDOE from the new UI's edit form; Bob cannot take it.
	w := post(admin, csrf, "/admin/students/1/edit", url.Values{"name": {"Alice"}, "group": {"A"}, "login": {"JDOE"}, "return": {"/admin/new/students/1"}})
	if msg, isErr := flashOf(w); isErr || w.Header().Get("Location") != "/admin/new/students/1" {
		t.Errorf("set login: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/students/3/edit", url.Values{"name": {"Bobby"}, "group": {"A"}, "login": {"jdoe"}})); !isErr || msg != "The login jdoe already belongs to Alice." {
		t.Errorf("taken login: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/students/3/edit", url.Values{"name": {"Bobby"}, "group": {"A"}, "login": {"bob smith"}})); !isErr || !strings.Contains(msg, "Latin letters") {
		t.Errorf("bad login: %q", msg)
	}
	var bobName string
	ts.s.DB.QueryRow("SELECT name FROM students WHERE id = 3").Scan(&bobName)
	if bobName != "Bob" {
		t.Errorf("a refused login still renamed Bob to %q", bobName)
	}
	// The old UI's form has no login field: the login stays.
	post(admin, csrf, "/admin/students/1/edit", url.Values{"name": {"Alice B"}, "group": {"A"}})
	var login string
	ts.s.DB.QueryRow("SELECT login FROM students WHERE id = 1").Scan(&login)
	if login != "JDOE" {
		t.Errorf("login after an old-UI edit: %q", login)
	}

	// Sync: 2 users; Alice (JDOE) is linked.
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); isErr || msg != "Synced: 2 platform users, 1 students linked." {
		t.Errorf("sync: %q %v", msg, isErr)
	}
	body := get(admin, "/admin/new/platform")
	if strings.Contains(body, token) {
		t.Fatal("the token is on the page")
	}
	for _, want := range []string{"access token saved", "2 platform users · students linked: 1 · logins not on the platform: 0 · students without a login: 2",
		`<td class="mono">jdoe</td>`, `<a class="name" href="/admin/new/students/1">Alice B</a>`, "Not linked to a student (1)", "<td class=\"num\">500</td>", "1.10"} {
		if !strings.Contains(body, want) {
			t.Errorf("platform page lacks %q", want)
		}
	}
	if b := get(admin, "/admin/new/platform?show=unlinked"); strings.Contains(b, `<td class="mono">jdoe</td>`) || !strings.Contains(b, `<td class="mono">asmith</td>`) {
		t.Error("unlinked filter")
	}
	if b := get(admin, "/admin/new/platform?sort=xp"); strings.Index(b, ">jdoe<") > strings.Index(b, ">asmith<") {
		t.Error("XP sort: most XP first")
	}
	if b := get(teach, "/admin/new/students/1"); !strings.Contains(b, "On the learning platform") || !strings.Contains(b, `<div class="k-value">500</div>`) || !strings.Contains(b, "Петров Иван") {
		t.Error("student page platform card")
	}
	if b := get(admin, "/admin/new/students/3"); !strings.Contains(b, "No platform login yet") {
		t.Error("student without a login")
	}
	if b := get(admin, "/admin/new/students?sort=login"); !strings.Contains(b, `<td class="mono">JDOE</td>`) {
		t.Error("login column in the students list")
	}

	// Link by name: "Smith Anna" ↔ asmith (Anna Smith).
	if msg, _ := flashOf(post(admin, csrf, "/admin/platform/link", url.Values{})); msg != "Students who got a login: 1. Smith Anna (asmith)" {
		t.Errorf("link by name: %q", msg)
	}
	if msg, _ := flashOf(post(admin, csrf, "/admin/platform/link", url.Values{})); msg != "No student could be linked by name unambiguously." {
		t.Errorf("link again: %q", msg)
	}
	var journal string
	ts.s.DB.QueryRow("SELECT group_concat(action || ':' || details, '|') FROM audit_log WHERE action LIKE 'platform%' OR action = 'set_login'").Scan(&journal)
	if strings.Contains(journal, token) || !strings.Contains(journal, "platform_sync") || !strings.Contains(journal, `"" -> "asmith"`) {
		t.Errorf("journal: %s", journal)
	}

	// A wrong token: the error is shown, without the token.
	if err := ts.s.syncer().SaveToken("wrongwrongwrong"); err != nil {
		t.Fatal(err)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); !isErr || !strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "wrongwrongwrong") {
		t.Errorf("wrong token: %q", msg)
	}
	if b := get(admin, "/admin/new/platform"); !strings.Contains(b, "failed: token request: HTTP 401") || strings.Contains(b, "wrongwrongwrong") {
		t.Error("the last error is not shown, or shows the token")
	}
}
