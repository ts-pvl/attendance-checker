package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"attendance/internal/core"
)

func (ts *testServer) upload(c *http.Cookie, csrf, path, week, name, content string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("csrf", csrf)
	mw.WriteField("week", week)
	if name != "" {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte(content))
	}
	mw.Close()
	r := httptest.NewRequest("POST", path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.RemoteAddr = staffIP + ":40000"
	r.AddCookie(c)
	w := httptest.NewRecorder()
	ts.h.ServeHTTP(w, r)
	return w
}

// Invented people: Петров Иван (jdoe, group A), Smith Anna (asmith, B),
// Ким Борис (bkim, A), and Мария Ли (mlee), a platform user only.
// Week of 2026-03-09, by hand:
//   - jdoe names asmith, bkim and himself (not counted);
//   - asmith names jdoe, mlee and "xyzq" (unresolved);
//   - bkim names "asmiht" (a typo for asmith) and jdoe.
//
// Counts: asmith 2, jdoe 2, bkim 1, mlee 1.
func TestHelpWeb(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	for _, st := range []struct{ name, group, login string }{{"Петров Иван", "A", "jdoe"}, {"Smith Anna", "B", "asmith"}, {"Ким Борис", "A", "bkim"}} {
		id, _ := core.AddStudent(ctx, ts.s.DB, ts.now, st.name, st.group, "test")
		core.SetLogin(ctx, ts.s.DB, ts.now, id, st.login, "test")
	}
	ts.s.DB.Exec("INSERT INTO platform_users(id, login, first_name, last_name, synced_at) VALUES(10, 'mlee', 'Мария', 'Ли', 0)")
	admin, csrf := ts.login("boss")
	teach, teachCSRF := ts.login("teach")
	get := func(c *http.Cookie, path string) string {
		w := ts.do(req{method: "GET", path: path, ip: staffIP, cookie: c})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	csv := "Timestamp,Ваше ФИО,Ваш Login (с платформы),Peer #1,Peer #2,Peer #3,Комментарий\n" +
		"1,Петров Иван,jdoe,asmith,bkim,jdoe,спасибо\n" +
		"2,Smith Anna,asmith,jdoe,mlee,xyzq,\n" +
		"3,Ким Борис,bkim,asmiht,jdoe,,\n"

	if b := get(admin, "/admin/new/help"); !strings.Contains(b, "No week has been imported yet.") || !strings.Contains(b, `name="week" value="2026-03-02"`) {
		t.Error("empty page, or the default week is not last week's Monday")
	}
	// No platform: no advice about loading its participants.
	if b := get(admin, "/admin/new/help"); strings.Contains(b, "Load all participants") {
		t.Error("cohort advice without a platform")
	}
	ts.s.Cfg.PlatformURL = "https://01.example.org"
	if b := get(admin, "/admin/new/help"); !strings.Contains(b, "Load all participants of the current piscine") {
		t.Error("no advice to load the cohort")
	}
	ts.s.DB.Exec("INSERT INTO settings(key, value) VALUES('platform_events', '10')")
	if b := get(admin, "/admin/new/help"); strings.Contains(b, "Load all participants") {
		t.Error("cohort advice with an event loaded")
	}
	if b := get(teach, "/admin/new/help"); strings.Contains(b, "/admin/help/import") {
		t.Error("teacher sees the import form")
	}
	if w := ts.upload(teach, teachCSRF, "/admin/help/import", "2026-03-11", "a.csv", csv); w.Code != http.StatusForbidden {
		t.Errorf("teacher imports: %d", w.Code)
	}
	if msg, _ := flashOf(ts.upload(admin, csrf, "/admin/help/import", "2026-03-11", "", "")); msg != "Choose the file with the answers." {
		t.Errorf("no file: %q", msg)
	}
	if msg, _ := flashOf(ts.upload(admin, csrf, "/admin/help/import", "2026-03-11", "a.csv", "name,login\nx,y\n")); !strings.HasPrefix(msg, "No peer columns found") {
		t.Errorf("no peer columns: %q", msg)
	}
	// Wednesday the 11th belongs to the week of Monday the 9th.
	w := ts.upload(admin, csrf, "/admin/help/import", "2026-03-11", "week1.csv", csv)
	if msg, isErr := flashOf(w); isErr || msg != "3 answers imported for the week of 2026-03-09." || w.Header().Get("Location") != "/admin/new/help?week=2026-03-09" {
		t.Errorf("import: %q %s", msg, w.Header().Get("Location"))
	}
	body := get(admin, "/admin/new/help?week=2026-03-09")
	order := func(b string) string {
		var out []string
		for _, part := range strings.Split(b, `<td class="mono">`)[1:] {
			out = append(out, part[:strings.Index(part, "<")])
		}
		return strings.Join(out, " ")
	}
	// Table (most named first; ties by all-weeks total, then login), then the review list.
	if got := order(body); got != "asmith jdoe bkim mlee xyzq asmiht" {
		t.Errorf("order: %s", got)
	}
	for _, want := range []string{"3 answers, 3 counted", "· 6 mentions counted · 1 unresolved",
		`<a class="name" href="/admin/new/students/2">Smith Anna</a>`, "Ли Мария <span class=\"pill\">not a student here</span>",
		"typo, corrected:</span> <span class=\"mono\">asmith</span>", `<span class="pill warn">not counted</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "спасибо") {
		t.Error("a comment was kept")
	}
	if got := order(get(admin, "/admin/new/help?week=2026-03-09&group=A")); got != "jdoe bkim xyzq asmiht" {
		t.Errorf("group A: %s", got)
	}
	// Staff resolve "xyzq" as bkim: bkim is named twice.
	if msg, isErr := flashOf(ts.do(req{method: "POST", path: "/admin/help/alias", ip: staffIP, cookie: admin,
		form: url.Values{"csrf": {csrf}, "typed": {"xyzq"}, "login": {"nobody"}}})); !isErr || msg != "No student or platform user has this login." {
		t.Errorf("alias to an unknown login: %q", msg)
	}
	ts.do(req{method: "POST", path: "/admin/help/alias", ip: staffIP, cookie: admin, form: url.Values{"csrf": {csrf}, "typed": {"xyzq"}, "login": {"BKIM"}}})
	if got := order(get(admin, "/admin/new/help?week=2026-03-09")); got != "asmith bkim jdoe mlee xyzq asmiht" {
		t.Errorf("after the alias: %s", got)
	}
	// Another week (the 2nd): bkim names jdoe. All weeks: jdoe 3.
	ts.upload(admin, csrf, "/admin/help/import", "2026-03-02", "week0.csv", "Login,Peer #1\nbkim,jdoe\n")
	all := get(admin, "/admin/new/help?week=all")
	if got := order(all); got != "jdoe asmith bkim mlee" || strings.Contains(all, "This week") {
		t.Errorf("all weeks: %s", got)
	}
	if b := get(teach, "/admin/new/students/1"); !strings.Contains(b, "3 times in all weeks") || !strings.Contains(b, `href="/admin/new/help?week=2026-03-09"`) {
		t.Error("student page")
	}
	// Re-import replaces; delete removes.
	if msg, _ := flashOf(ts.upload(admin, csrf, "/admin/help/import", "2026-03-04", "week0b.csv", "Login,Peer #1\nbkim,asmith\n")); !strings.Contains(msg, "was replaced") {
		t.Errorf("re-import: %q", msg)
	}
	if msg, _ := flashOf(ts.do(req{method: "POST", path: "/admin/help/delete", ip: staffIP, cookie: admin, form: url.Values{"csrf": {csrf}, "week": {"2026-03-02"}}})); msg != "The week is deleted." {
		t.Errorf("delete: %q", msg)
	}
	if b := get(admin, "/admin/new/help"); strings.Contains(b, "Week of 2026-03-02") || !strings.Contains(b, "Week of 2026-03-09") {
		t.Error("weeks after the delete")
	}
	var journal int
	ts.s.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action LIKE 'help%' AND actor = 'boss'").Scan(&journal)
	if journal != 5 { // 3 imports, 1 alias, 1 delete
		t.Errorf("journal: %d", journal)
	}
}
