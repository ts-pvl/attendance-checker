package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// flashOf reads the message a POST leaves for the next page.
func flashOf(w *httptest.ResponseRecorder) (string, bool) {
	for _, c := range w.Result().Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			b, _ := base64.RawURLEncoding.DecodeString(c.Value)
			var f flash
			json.Unmarshal(b, &f)
			return f.Msg, f.Err
		}
	}
	return "", false
}

// todayScenario at 10:00: Alice is checked in on PC-01; on PC-02 someone is
// logged in without a check-in; Bob, Carol and Dave are not here. So
// "everyone present" is Alice alone.
func TestPointsWeb(t *testing.T) {
	ts := todayScenario(t)
	admin, csrf := ts.login("boss")
	teach, teachCSRF := ts.login("teach")
	post := func(c *http.Cookie, token, path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", token)
		return ts.do(req{method: "POST", path: path, ip: staffIP, cookie: c, form: form})
	}
	get := func(c *http.Cookie, path string) string {
		w := ts.do(req{method: "GET", path: path, ip: staffIP, cookie: c})
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}

	// The Today tab: the form for admins only.
	if b := get(admin, "/admin/today"); !strings.Contains(b, `action="/admin/points/present"`) {
		t.Error("no points form for the admin")
	}
	if b := get(teach, "/admin/today"); strings.Contains(b, `action="/admin/points/present"`) || strings.Contains(b, "Social activity points") {
		t.Error("teacher sees the points form or an empty points card")
	}
	if w := post(teach, teachCSRF, "/admin/points/present", url.Values{"points": {"5"}, "reason": {"x"}}); w.Code != http.StatusForbidden {
		t.Errorf("teacher gives points: %d", w.Code)
	}
	if w := post(admin, "wrong", "/admin/points/present", url.Values{"points": {"5"}, "reason": {"x"}}); w.Code != http.StatusForbidden {
		t.Errorf("bad CSRF token: %d", w.Code)
	}

	w := post(admin, csrf, "/admin/points/present", url.Values{"points": {"5"}, "reason": {"Cleaned the lab"}, "return": {"/admin/today?sort=pc"}})
	if msg, isErr := flashOf(w); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/today?sort=pc" || isErr || msg != "5 points added to each of 1 students." {
		t.Errorf("award: %d %s %q %v", w.Code, w.Header().Get("Location"), msg, isErr)
	}
	for _, f := range []url.Values{{"points": {"0"}, "reason": {"x"}}, {"points": {"abc"}, "reason": {"x"}}, {"points": {"1001"}, "reason": {"x"}}} {
		if msg, isErr := flashOf(post(admin, csrf, "/admin/points/present", f)); !isErr || msg != "Points must be a whole number from 1 to 1000." {
			t.Errorf("%v: %q", f, msg)
		}
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/points/present", url.Values{"points": {"3"}, "reason": {"  "}})); !isErr || msg != "Write what the points are for." {
		t.Errorf("no reason: %q", msg)
	}
	body := get(admin, "/admin/today")
	if !strings.Contains(body, "Given to everyone present today") || !strings.Contains(body, "Cleaned the lab") || !strings.Contains(body, `action="/admin/points/batch/1/cancel"`) {
		t.Error("today's award is not listed with a cancel button")
	}
	// Teachers see the list, without the buttons.
	if b := get(teach, "/admin/today"); !strings.Contains(b, "Cleaned the lab") || strings.Contains(b, "/cancel") {
		t.Error("teacher's view of today's awards")
	}

	// One student: Bob (id 2) gets 3; the student page shows 3 and Alice's page 5.
	w = post(admin, csrf, "/admin/students/2/points", url.Values{"points": {"3"}, "reason": {"Helped at the open day"}, "return": {"/admin/students/2"}})
	if msg, _ := flashOf(w); w.Header().Get("Location") != "/admin/students/2" || msg != "3 points added." {
		t.Errorf("one student: %s %q", w.Header().Get("Location"), msg)
	}
	if b := get(admin, "/admin/students/2"); !strings.Contains(b, "Total: 3") || !strings.Contains(b, "Helped at the open day") || !strings.Contains(b, `action="/admin/students/2/points"`) {
		t.Error("Bob's page")
	}
	if b := get(teach, "/admin/students/1"); !strings.Contains(b, "Total: 5") || !strings.Contains(b, "everyone present") || strings.Contains(b, `action="/admin/students/1/points"`) {
		t.Error("Alice's page as a teacher")
	}
	if w := post(teach, teachCSRF, "/admin/students/2/points", url.Values{"points": {"3"}, "reason": {"x"}}); w.Code != http.StatusForbidden {
		t.Errorf("teacher gives one student points: %d", w.Code)
	}

	// Cancelling: the batch (Alice back to 0), then Bob's entry.
	ts.now = ts.now.Add(time.Minute)
	if msg, _ := flashOf(post(admin, csrf, "/admin/points/batch/1/cancel", url.Values{})); msg != "Cancelled for 1 students." {
		t.Errorf("cancel batch: %q", msg)
	}
	if msg, isErr := flashOf(post(admin, csrf, "/admin/points/batch/1/cancel", url.Values{})); !isErr || msg != "These points are already cancelled." {
		t.Errorf("cancel batch again: %q", msg)
	}
	if b := get(admin, "/admin/students/1"); !strings.Contains(b, "Total: 0") || !strings.Contains(b, "cancelled by boss") {
		t.Error("Alice after the cancel")
	}
	var bobEntry string
	ts.s.DB.QueryRow("SELECT id FROM points WHERE student_id = 2").Scan(&bobEntry)
	w = post(admin, csrf, "/admin/points/"+bobEntry+"/cancel", url.Values{})
	if msg, _ := flashOf(w); w.Header().Get("Location") != "/admin/students/2" || msg != "The points are cancelled." {
		t.Errorf("cancel Bob's entry: %s %q", w.Header().Get("Location"), msg)
	}
	if b := get(admin, "/admin/today"); !strings.Contains(b, `<span class="pill">cancelled</span>`) {
		t.Error("the cancelled batch is not marked")
	}
}
