package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"attendance/internal/platform"
)

// A small fake platform (invented people): jdoe (Иван Петров, also Ivan
// Petrov, audit 1.1) and asmith (Anna Smith) are in event 10, a piscine:
// jdoe at level 5 with 300 + 200 XP, asmith at level 2 with 50 XP.
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
			var body struct {
				Query     string
				Variables struct {
					IDs    []int64  `json:"ids"`
					Logins []string `json:"logins"`
					After  int64    `json:"after"`
				}
			}
			json.NewDecoder(r.Body).Decode(&body)
			q, v := body.Query, body.Variables
			users := map[int64]string{
				1: `{"id":1,"login":"jdoe","firstName":"Ivan","lastName":"Petrov","email":"jdoe@example.org","auditRatio":1.1,
					"firstNameCyr":"Иван","lastNameCyr":"Петров","firstNameLat":"Ivan","lastNameLat":"Petrov"}`,
				2: `{"id":2,"login":"asmith","firstName":"Anna","lastName":"Smith","email":null,"auditRatio":null}`,
			}
			members := map[int64]string{1: `{"id":1,"userId":1,"eventId":10,"level":5}`, 2: `{"id":2,"userId":2,"eventId":10,"level":2}`}
			xp := map[int64][]string{
				1: {`{"id":1,"userId":1,"eventId":10,"amount":300,"createdAt":"2026-03-03T10:00:00+00:00"}`,
					`{"id":2,"userId":1,"eventId":10,"amount":200,"createdAt":"2026-03-04T10:00:00+00:00"}`},
				2: {`{"id":3,"userId":2,"eventId":10,"amount":50,"createdAt":"2026-03-04T11:00:00+00:00"}`},
			}
			var rows []string
			switch {
			case strings.Contains(q, "user(where: {login:"):
				for _, l := range v.Logins {
					switch l {
					case "jdoe":
						rows = append(rows, `{"id":1}`)
					case "asmith":
						rows = append(rows, `{"id":2}`)
					}
				}
				fmt.Fprintf(w, `{"data":{"user":[%s]}}`, strings.Join(rows, ","))
			case strings.Contains(q, "user(where: {id:"):
				for _, id := range v.IDs {
					if u, ok := users[id]; ok {
						rows = append(rows, u)
					}
				}
				fmt.Fprintf(w, `{"data":{"user":[%s]}}`, strings.Join(rows, ","))
			case strings.Contains(q, "event(where: {id:"):
				if slices.Contains(v.IDs, 10) {
					rows = append(rows, `{"id":10,"path":"/campus/piscinego","parentId":null,"startAt":"2026-03-02T05:00:00+00:00","endAt":"2026-03-27T13:00:00+00:00"}`)
				}
				fmt.Fprintf(w, `{"data":{"event":[%s]}}`, strings.Join(rows, ","))
			case v.After > 0: // every list fits in one page
			case strings.Contains(q, "event_user(where: {eventId:"):
				if slices.Contains(v.IDs, 10) {
					rows = append(rows, members[1], members[2])
				}
			case strings.Contains(q, "event_user(where: {userId:"):
				for _, id := range v.IDs {
					if m, ok := members[id]; ok {
						rows = append(rows, m)
					}
				}
			case strings.Contains(q, "transaction(where:"):
				for _, id := range v.IDs {
					rows = append(rows, xp[id]...)
				}
			default:
				t.Errorf("unexpected query: %s", q)
			}
			if strings.Contains(q, "rows:") {
				fmt.Fprintf(w, `{"data":{"rows":[%s]}}`, strings.Join(rows, ","))
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
	ts.s.Cfg.PlatformURL = srv.URL
	ts.s.Cfg.PlatformTokenFile = filepath.Join(t.TempDir(), "platform_token")
	ts.s.syncer().HTTP = srv.Client()
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

	// The address comes from the configuration: shown, never asked for.
	if b := get(admin, "/admin/platform"); !strings.Contains(b, "Save the access token under Settings to start.") ||
		!strings.Contains(b, `action="/admin/platform/settings"`) || !strings.Contains(b, srv.URL) || strings.Contains(b, `name="url"`) {
		t.Error("platform page before setup")
	}
	if b := get(teach, "/admin/platform"); strings.Contains(b, "/admin/platform/") {
		t.Error("teacher sees platform actions")
	}
	for _, p := range []string{"/admin/platform/settings", "/admin/platform/sync", "/admin/platform/link", "/admin/platform/event"} {
		if w := post(teach, teachCSRF, p, url.Values{"event": {"10"}, "load": {"1"}}); w.Code != http.StatusForbidden {
			t.Errorf("teacher posts %s: %d", p, w.Code)
		}
	}
	if msg, _ := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); msg != "no access token saved" {
		t.Errorf("sync before the token: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/settings", url.Values{"events": {""}, "token": {token}})); isErr || msg != "Platform settings saved." {
		t.Errorf("save: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); !isErr || !strings.HasPrefix(msg, "Nothing to read") {
		t.Errorf("sync without logins: %q", msg)
	}

	// Students, the first with a login from the start (the new UI's form).
	add := func(name, group, login string) (string, bool) {
		return flashOf(post(admin, csrf, "/admin/students/add", url.Values{"name": {name}, "group": {group}, "login": {login}, "return": {"/admin/students"}}))
	}
	if msg, isErr := add("Alice", "A", "JDOE"); isErr {
		t.Fatalf("add with a login: %q", msg)
	}
	if msg, isErr := add("Bobby", "A", "jdoe"); !isErr || msg != "The login jdoe already belongs to Alice." {
		t.Errorf("add with a taken login: %q", msg)
	}
	if msg, isErr := add("Bobby", "A", "bob smith"); !isErr || !strings.Contains(msg, "Latin letters") {
		t.Errorf("add with a bad login: %q", msg)
	}
	add("Smith Anna", "B", "") // 2
	add("Bob", "A", "")        // 3
	var n int
	ts.s.DB.QueryRow("SELECT COUNT(*) FROM students").Scan(&n)
	if n != 3 {
		t.Errorf("%d students, want 3: a refused login still added one", n)
	}
	for _, page := range []string{"/admin/students", "/admin/students"} {
		if b := get(admin, page); !strings.Contains(b, `name="login"`) {
			t.Errorf("%s: no login field in the add form", page)
		}
	}

	// Logins after: Bob cannot take Alice's; a refused login changes nothing.
	if msg, isErr := flashOf(post(admin, csrf, "/admin/students/3/edit", url.Values{"name": {"Bobby"}, "group": {"A"}, "login": {"jdoe"}})); !isErr || msg != "The login jdoe already belongs to Alice." {
		t.Errorf("taken login: %q", msg)
	}
	var bobName string
	ts.s.DB.QueryRow("SELECT name FROM students WHERE id = 3").Scan(&bobName)
	if bobName != "Bob" {
		t.Errorf("a refused login still renamed Bob to %q", bobName)
	}
	// The old UI's edit form has no login field: the login stays.
	post(admin, csrf, "/admin/students/1/edit", url.Values{"name": {"Alice B"}, "group": {"A"}})
	var login string
	ts.s.DB.QueryRow("SELECT login FROM students WHERE id = 1").Scan(&login)
	if login != "JDOE" {
		t.Errorf("login after an old-UI edit: %q", login)
	}

	// Sync by the logins alone: jdoe.
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); isErr || msg != "Synced: 1 platform users, 1 students linked." {
		t.Errorf("sync: %q %v", msg, isErr)
	}
	body := get(admin, "/admin/platform")
	for _, want := range []string{"the students&#39; logins are read", `<span class="mono">piscinego</span>`, `name="load" value="1"`, "Load all participants",
		`<td class="mono">jdoe</td>`, `<td class="num">5</td>`, `title="500">500 B</td>`, "Ivan Petrov"} {
		if !strings.Contains(body, want) {
			t.Errorf("platform page (logins) lacks %q", want)
		}
	}

	// Load all of event 10: asmith too.
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/event", url.Values{"event": {"10"}, "load": {"1"}})); isErr || !strings.HasPrefix(msg, "Saved.") {
		t.Errorf("load event 10: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); isErr || msg != "Synced: 2 platform users, 1 students linked." {
		t.Errorf("sync with event 10: %q %v", msg, isErr)
	}
	body = get(admin, "/admin/platform")
	if strings.Contains(body, token) {
		t.Fatal("the token is on the page")
	}
	for _, want := range []string{"access token saved", "events 10", "2 platform users · students linked: 1 · logins not on the platform: 0 · students without a login: 2",
		`<a class="name" href="/admin/students/1">Alice B</a>`, "Not linked to a student (1)", "Stop loading all", "all participants", "1.10"} {
		if !strings.Contains(body, want) {
			t.Errorf("platform page (event) lacks %q", want)
		}
	}
	if b := get(admin, "/admin/platform?show=unlinked"); strings.Contains(b, `<td class="mono">jdoe</td>`) || !strings.Contains(b, `<td class="mono">asmith</td>`) {
		t.Error("unlinked filter")
	}
	for _, key := range []string{"xp", "level"} {
		if b := get(admin, "/admin/platform?sort="+key); strings.Index(b, ">jdoe<") > strings.Index(b, ">asmith<") {
			t.Errorf("%s sort: the highest first", key)
		}
	}
	b := get(teach, "/admin/students/1")
	for _, want := range []string{"On the learning platform", `<div class="k-value">5</div>`, `title="500">500 B</div>`, "Петров Иван", "Ivan Petrov",
		`<span class="pill">current</span>`, "2026-03-02 – 2026-03-27"} {
		if !strings.Contains(b, want) {
			t.Errorf("student page lacks %q", want)
		}
	}
	if strings.Contains(b, "/admin/platform/sync") {
		t.Error("a teacher sees Sync now on the student page")
	}
	if b := get(admin, "/admin/students/1"); !strings.Contains(b, `action="/admin/platform/sync"`) {
		t.Error("an admin has no Sync now on the student page")
	}
	if b := get(admin, "/admin/students/3"); !strings.Contains(b, "No platform login yet") {
		t.Error("student without a login")
	}
	if b := get(admin, "/admin/students?sort=login"); !strings.Contains(b, `<td class="mono">JDOE</td>`) {
		t.Error("login column in the students list")
	}

	// Link by name: "Smith Anna" ↔ asmith (Anna Smith).
	if msg, _ := flashOf(post(admin, csrf, "/admin/platform/link", url.Values{})); msg != "Students who got a login: 1. Smith Anna (asmith)" {
		t.Errorf("link by name: %q", msg)
	}
	if msg, _ := flashOf(post(admin, csrf, "/admin/platform/link", url.Values{})); msg != "No student could be linked by name unambiguously." {
		t.Errorf("link again: %q", msg)
	}
	// A login the platform does not know.
	post(admin, csrf, "/admin/students/3/edit", url.Values{"name": {"Bob"}, "group": {"A"}, "login": {"nobody1"}})
	post(admin, csrf, "/admin/platform/sync", url.Values{})
	if b := get(admin, "/admin/students/3"); !strings.Contains(b, "The login nobody1 was not found on the platform at the last sync") {
		t.Error("unknown login on the student page")
	}
	var journal string
	ts.s.DB.QueryRow("SELECT group_concat(action || ':' || details, '|') FROM audit_log WHERE action LIKE 'platform%' OR action IN ('set_login', 'add_student')").Scan(&journal)
	for _, want := range []string{"platform_sync", `"" -> "asmith"`, `events "10"`, `Alice, login "JDOE"`} {
		if !strings.Contains(journal, want) {
			t.Errorf("journal lacks %q: %s", want, journal)
		}
	}
	if strings.Contains(journal, token) {
		t.Error("the token is in the journal")
	}

	// Stop loading the event.
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/event", url.Values{"event": {"10"}, "load": {"0"}})); isErr || !strings.HasPrefix(msg, "Saved.") {
		t.Errorf("stop loading: %q", msg)
	}
	if set, _ := platform.LoadSettings(ctx, ts.s.DB); len(set.Events) != 0 {
		t.Errorf("events after stopping: %v", set.Events)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/event", url.Values{"event": {"x"}, "load": {"1"}})); !isErr {
		t.Errorf("bad event number: %q", msg)
	}

	// A wrong token: the error is shown, without the token.
	if err := ts.s.syncer().SaveToken("wrongwrongwrong"); err != nil {
		t.Fatal(err)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/platform/sync", url.Values{})); !isErr || !strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "wrongwrongwrong") {
		t.Errorf("wrong token: %q", msg)
	}
	if b := get(admin, "/admin/platform"); !strings.Contains(b, "failed: token request: HTTP 401") || strings.Contains(b, "wrongwrongwrong") {
		t.Error("the last error is not shown, or shows the token")
	}

	// No platform_url: nothing can sync.
	ts.s.Cfg.PlatformURL, ts.s.Platform = "", nil
	if b := get(admin, "/admin/platform"); !strings.Contains(b, "platform_url") {
		t.Error("an empty platform_url is not explained")
	}
}
