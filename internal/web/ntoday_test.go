package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"attendance/internal/core"
)

// todayScenario builds one morning through the real path (PC events, QR
// scans), in time order. The clock is 2026-03-10 10:00. Five PCs:
//
//	PC-01  login 08:00, Alice (A) scans 08:05, still working
//	PC-02  Carol (B) 08:00 → logout 08:20; a new login at 09:30, nobody scans
//	PC-03  yesterday: Carol 14:00 → 15:00; today: Bob (A) 07:10 → logout 09:15
//	PC-04  Dave (B) yesterday 23:00 → logout today 00:30
//	PC-05  never used
//
// By hand, at 10:00:
//
//	PC-01 busy, Alice, 1 h 55 min     PC-02 logged in without check-in, 30 min
//	PC-03 free for 45 min             PC-04 free for 9 h 30 min     PC-05 no session yet
//
// Sessions that ended today, most recent first:
//
//	PC-03 Bob   A  2 h 05 min  ended 09:15 (45 min ago)
//	PC-02 Carol B  20 min      ended 08:20 (1 h 40 min ago)
//	PC-04 Dave  B  1 h 30 min  ended 00:30 (9 h 30 min ago)
//
// Carol's visit yesterday afternoon ended yesterday: not in the list.
func todayScenario(t *testing.T) *testServer {
	ts := newTestServer(t)
	ctx := context.Background()
	var entries []core.HostEntry
	for i := 1; i <= 5; i++ {
		entries = append(entries, core.HostEntry{Name: fmt.Sprintf("PC-%02d", i), IP: netip.MustParseAddr(fmt.Sprintf("10.0.5.%d", 10+i))})
	}
	if err := ts.s.Hosts.Sync(ctx, ts.s.DB.DB, entries); err != nil {
		t.Fatal(err)
	}
	for i, st := range []struct{ name, group string }{{"Alice", "A"}, {"Bob", "A"}, {"Carol", "B"}, {"Dave", "B"}} {
		id, _ := core.AddStudent(ctx, ts.s.DB, ts.now, st.name, st.group, "test")
		code, _ := core.GenerateCode(ctx, ts.s.DB, ts.now, id, time.Hour, "test")
		core.Enroll(ctx, ts.s.DB, ts.now, core.TGUser{ID: int64(501 + i)}, code)
	}
	const alice, bob, carol, dave = 501, 502, 503, 504
	loc := ts.s.Cfg.Timezone
	at := func(day int, hm string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("2026-03-%02d %s", day, hm), loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	type step struct {
		at time.Time
		do func(time.Time)
	}
	var steps []step
	agent := func(tm time.Time, pc, kind, sid string) {
		steps = append(steps, step{tm, func(tm time.Time) {
			if _, err := ts.s.Engine.AgentEvent(ctx, tm, pc, kind, sid, sql.NullInt64{Int64: 0, Valid: kind != core.KindSessionStart}); err != nil {
				t.Fatal(err)
			}
		}})
	}
	scan := func(tm time.Time, pc string, tg int64) {
		steps = append(steps, step{tm, func(tm time.Time) {
			h, _ := ts.s.Hosts.ByName(pc)
			tok, _ := ts.s.Board.Current(h.ID, tm.Add(-5*time.Second))
			if r, err := ts.s.Checkin.Scan(ctx, tm, tg, tok); err != nil || r.Result != core.ScanOK {
				t.Fatalf("scan on %s: %v %+v", pc, err, r)
			}
		}})
	}
	// session: login at from, a heartbeat every minute, logout at to (a zero
	// "to" leaves it open, with heartbeats up to the current time).
	session := func(pc, sid string, from, to time.Time) {
		agent(from, pc, core.KindSessionStart, sid)
		last := to
		if to.IsZero() {
			last = ts.now.Add(time.Second)
		}
		for tm := from.Add(time.Minute); tm.Before(last); tm = tm.Add(time.Minute) {
			agent(tm, pc, core.KindHeartbeat, sid)
		}
		if !to.IsZero() {
			agent(to, pc, core.KindLogout, sid)
		}
	}
	session("PC-03", "s1", at(9, "14:00"), at(9, "15:00"))
	scan(at(9, "14:00"), "PC-03", carol)
	session("PC-04", "s2", at(9, "23:00"), at(10, "00:30"))
	scan(at(9, "23:00"), "PC-04", dave)
	session("PC-03", "s3", at(10, "07:00"), at(10, "09:15"))
	scan(at(10, "07:10"), "PC-03", bob)
	session("PC-02", "s4", at(10, "08:00"), at(10, "08:20"))
	scan(at(10, "08:00"), "PC-02", carol)
	session("PC-01", "s5", at(10, "08:00"), time.Time{})
	scan(at(10, "08:05"), "PC-01", alice)
	session("PC-02", "s6", at(10, "09:30"), time.Time{})
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at.Before(steps[j].at) })
	for _, st := range steps {
		st.do(st.at)
	}
	return ts
}

func TestNewToday(t *testing.T) {
	ts := todayScenario(t)
	ctx := context.Background()
	h, m := time.Hour, time.Minute

	d, err := ts.s.newToday(ctx, parseRecentOpts(url.Values{}))
	if err != nil {
		t.Fatal(err)
	}
	// The default grid: 5 rows of 10 cells; the five PCs in name order, then empty cells.
	if len(d.Rows) != 5 || len(d.Rows[0]) != 10 || d.Custom {
		t.Fatalf("grid: %d rows, custom %v", len(d.Rows), d.Custom)
	}
	want := []pcTile{
		{Index: 0, Host: "PC-01", State: pcBusy, StudentID: 1, Student: "Alice", For: 1*h + 55*m},
		{Index: 1, Host: "PC-02", State: pcLogin, For: 30 * m},
		{Index: 2, Host: "PC-03", State: pcFree, For: 45 * m, Known: true},
		{Index: 3, Host: "PC-04", State: pcFree, For: 9*h + 30*m, Known: true},
		{Index: 4, Host: "PC-05", State: pcFree},
		{Index: 5},
	}
	for i, w := range want {
		if got := d.Rows[0][i]; got != w {
			t.Errorf("cell %d: %+v, want %+v", i, got, w)
		}
	}
	if d.Rows[4][9] != (pcTile{Index: 49}) {
		t.Errorf("last cell: %+v", d.Rows[4][9])
	}
	if d.Busy != 1 || d.NoCheckin != 1 || d.Free != 3 {
		t.Errorf("busy %d, no check-in %d, free %d; want 1, 1, 3", d.Busy, d.NoCheckin, d.Free)
	}

	// The table: order, lengths and end times for every sort.
	list := func(query string) (string, recentView) {
		q, _ := url.ParseQuery(query)
		d, err := ts.s.newToday(ctx, parseRecentOpts(q))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range d.Table.Rows {
			out = append(out, r.Student.Name)
		}
		return strings.Join(out, ","), d.Table
	}
	got, v := list("")
	if got != "Bob,Carol,Dave" {
		t.Errorf("default order (most recent first): %s", got)
	}
	if len(v.Rows) == 3 {
		wantRows := []struct {
			host, group string
			length      time.Duration
			end         string
		}{{"PC-03", "A", 2*h + 5*m, "09:15"}, {"PC-02", "B", 20 * m, "08:20"}, {"PC-04", "B", 1*h + 30*m, "00:30"}}
		for i, w := range wantRows {
			r := v.Rows[i]
			if r.Host != w.host || r.Student.Group != w.group || r.Len != w.length || r.End.In(ts.s.Cfg.Timezone).Format("15:04") != w.end {
				t.Errorf("row %d: %s %s %v %v, want %+v", i, r.Host, r.Student.Group, r.Len, r.End, w)
			}
		}
	}
	if v.Self != "/admin/new/today" || v.Pager.Total != 3 || len(v.Pager.Links) != 0 {
		t.Errorf("default view: self %q, total %d", v.Self, v.Pager.Total)
	}
	for query, want := range map[string]string{
		"sort=ago&dir=desc": "Dave,Carol,Bob", // longest ago first
		"sort=pc":           "Carol,Bob,Dave", // PC-02, PC-03, PC-04
		"sort=pc&dir=desc":  "Dave,Bob,Carol",
		"sort=len":          "Bob,Dave,Carol", // first click: longest first (125, 90, 20 min)
		"sort=len&dir=asc":  "Carol,Dave,Bob",
		"sort=nonsense":     "Bob,Carol,Dave",
		"group=B":           "Carol,Dave",
		"group=A&sort=len":  "Bob",
		"per=2":             "Bob,Carol",
		"per=2&page=2":      "Dave",
		"per=2&page=9":      "Dave", // past the end: the last page
		"per=all":           "Bob,Carol,Dave",
		"per=1&sort=pc":     "Carol",
	} {
		if got, _ := list(query); got != want {
			t.Errorf("%s: %s, want %s", query, got, want)
		}
	}
	if _, v := list("per=2&page=2&group=&sort=len&dir=asc"); v.Self != "/admin/new/today?dir=asc&page=2&per=2&sort=len" ||
		v.Pager.Prev != "/admin/new/today?dir=asc&per=2&sort=len" || v.Pager.Next != "" || v.Pager.First != 3 || v.Pager.Last != 3 {
		t.Errorf("page 2 of 2: self %q, pager %+v", v.Self, v.Pager)
	}
	// The column heads: PC, length and elapsed time sort; the active one flips.
	_, v = list("sort=len")
	heads := map[string]tableHead{}
	for _, hd := range v.Heads {
		heads[hd.Key] = hd
	}
	if len(v.Heads) != 5 || heads["len"].Sort != "descending" || heads["len"].URL != "/admin/new/today?dir=asc&sort=len" ||
		heads["pc"].URL != "/admin/new/today?dir=asc&sort=pc" || heads["pc"].Sort != "" || heads["ago"].URL != "/admin/new/today" {
		t.Errorf("heads: %+v", v.Heads)
	}
	// A group with no session today, and a visit deleted by staff.
	if got, v := list("group=Z"); got != "" || !v.Any {
		t.Errorf("unknown group: %q, any %v", got, v.Any)
	}
	var bobVisit int64
	ts.s.DB.QueryRow("SELECT id FROM presence WHERE student_id = 2").Scan(&bobVisit)
	if _, err := core.AddCorrection(ctx, ts.s.DB, ts.now, core.CorrectionInput{Action: "delete", Target: fmt.Sprintf("p%d", bobVisit),
		StudentID: 2, Reason: "test", Actor: "boss"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := list(""); got != "Carol,Dave" {
		t.Errorf("after deleting Bob's visit: %s", got)
	}

	// Half an hour later nothing else has happened: every time moves on by 30 min.
	for i := 0; i < 30; i++ {
		ts.now = ts.now.Add(m)
		ts.s.Engine.AgentEvent(ctx, ts.now, "PC-01", core.KindHeartbeat, "s5", sql.NullInt64{Valid: true})
		ts.s.Engine.AgentEvent(ctx, ts.now, "PC-02", core.KindHeartbeat, "s6", sql.NullInt64{Valid: true})
	}
	d, _ = ts.s.newToday(ctx, parseRecentOpts(url.Values{}))
	if a, b, c := d.Rows[0][0].For, d.Rows[0][1].For, d.Rows[0][2].For; a != 2*h+25*m || b != 1*h || c != 1*h+15*m {
		t.Errorf("at 10:30: %v %v %v, want 2h25m, 1h, 1h15m", a, b, c)
	}
}

// At 00:10 the next day nothing has ended "today" yet.
func TestNewTodayIsTheLocalDay(t *testing.T) {
	ts := todayScenario(t)
	ts.now = time.Date(2026, 3, 11, 0, 10, 0, 0, ts.s.Cfg.Timezone)
	d, err := ts.s.newToday(context.Background(), parseRecentOpts(url.Values{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Table.Rows) != 0 || d.Table.Any {
		t.Errorf("rows after midnight: %+v", d.Table.Rows)
	}
}

func TestNewTodayPage(t *testing.T) {
	ts := todayScenario(t)
	if w := ts.do(req{method: "GET", path: "/admin/new/today", ip: staffIP}); w.Code != http.StatusSeeOther {
		t.Errorf("without login: %d", w.Code)
	}
	admin, csrf := ts.login("boss")
	teach, teachCSRF := ts.login("teach")
	page := func(c *http.Cookie, path string) string {
		w := ts.do(req{method: "GET", path: path, ip: staffIP, cookie: c})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	hostRe := regexp.MustCompile(`<span class="pc-name">([^<]+)</span>`)
	order := func(body string) string {
		var out []string
		for _, m := range hostRe.FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return strings.Join(out, " ")
	}
	body := page(admin, "/admin/new/today")
	for _, want := range []string{
		`aria-current="page">Today</a>`, `<script src="/static/today.js" defer></script>`,
		"With a student: 1 · logged in without a check-in: 1 · free: 3",
		`<div class="pc busy" draggable="true" data-host="PC-01">`, `href="/admin/new/students/1"`, "1 h 55 min",
		`<div class="pc login" draggable="true" data-host="PC-02">`, "30 min",
		"9 h 30 min", "no sessions yet",
		`hx-get="/admin/new/today" hx-trigger="every 30s"`,
		`45 min ago <small class="muted">09:15</small>`, "2 h 05 min",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if got := order(body); got != "PC-01 PC-02 PC-03 PC-04 PC-05" {
		t.Errorf("default order: %s", got)
	}
	if n := strings.Count(body, `class="pcrow"`); n != 5 || strings.Count(body, "data-cell=") != 50 {
		t.Errorf("default grid: %d rows, %d cells", n, strings.Count(body, "data-cell="))
	}
	if strings.Contains(body, "style=") || strings.Contains(body, "<style") || strings.Contains(body, "<script>") {
		t.Error("inline style or script on the page")
	}
	// The reload address carries the table settings.
	if b := page(admin, "/admin/new/today?sort=pc&group=B&per=10"); !strings.Contains(b, `hx-get="/admin/new/today?group=B&amp;per=10&amp;sort=pc&amp;dir=asc"`) &&
		!strings.Contains(b, `hx-get="/admin/new/today?dir=asc&amp;group=B&amp;per=10&amp;sort=pc"`) {
		t.Error("the reload address lost the table settings")
	}
	// Teachers see the page but cannot change the grid.
	tb := page(teach, "/admin/new/today")
	if strings.Contains(tb, "draggable=\"true\"") || strings.Contains(tb, "/admin/new/today/layout") || !strings.Contains(tb, `<div class="pc busy">`) {
		t.Error("teacher view: grid controls shown or tiles missing")
	}
	post := func(c *http.Cookie, token string, form url.Values) int {
		if token != "" {
			form.Set("csrf", token)
		}
		return ts.do(req{method: "POST", path: "/admin/new/today/layout", ip: staffIP, cookie: c, form: form}).Code
	}
	if code := post(teach, teachCSRF, url.Values{"action": {"reset"}}); code != http.StatusForbidden {
		t.Errorf("teacher changes the grid: %d", code)
	}
	if code := post(admin, "", url.Values{"action": {"reset"}}); code != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", code)
	}

	// Swap PC-01 with the cell of PC-03 (cell 2), then move PC-05 to the empty cell 12.
	if code := post(admin, csrf, url.Values{"action": {"swap"}, "host": {"PC-01"}, "to": {"2"}}); code != http.StatusSeeOther {
		t.Fatalf("swap: %d", code)
	}
	if got := order(page(admin, "/admin/new/today")); got != "PC-03 PC-02 PC-01 PC-04 PC-05" {
		t.Errorf("after the swap: %s", got)
	}
	post(admin, csrf, url.Values{"action": {"swap"}, "host": {"PC-05"}, "to": {"12"}})
	body = page(admin, "/admin/new/today")
	if got := order(body); got != "PC-03 PC-02 PC-01 PC-04 PC-05" || !strings.Contains(body, `<div class="pccell empty" data-cell="4">`) ||
		!regexp.MustCompile(`<div class="pccell" data-cell="12">\s*<div class="pc free" draggable="true" data-host="PC-05">`).MatchString(body) {
		t.Errorf("after the move: %s", got)
	}
	// 2 rows of 3. The grid was PC-03 PC-02 PC-01 PC-04 _ … with PC-05 in cell
	// 12 (second row, third column). PC-03, PC-02, PC-01 and PC-05 are inside
	// the new grid and stay; PC-04 (fourth column) takes the first empty cell:
	//   PC-03 PC-02 PC-01 / PC-04 _ PC-05
	w := ts.do(req{method: "POST", path: "/admin/new/today/layout", ip: staffIP, cookie: admin,
		form: url.Values{"csrf": {csrf}, "action": {"resize"}, "rows": {"2"}, "cols": {"3"}, "return": {"/admin/new/today?sort=pc"}}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/new/today?sort=pc" {
		t.Errorf("resize: %d to %s", w.Code, w.Header().Get("Location"))
	}
	body = page(admin, "/admin/new/today")
	if got := order(body); got != "PC-03 PC-02 PC-01 PC-04 PC-05" || strings.Count(body, `class="pcrow"`) != 2 || strings.Count(body, "data-cell=") != 6 ||
		!strings.Contains(body, `<div class="pccell empty" data-cell="4">`) ||
		!strings.Contains(body, `name="rows" min="1" max="30" value="2"`) || !strings.Contains(body, `name="cols" min="1" max="30" value="3"`) {
		t.Errorf("after 2x3: %s, %d rows", got, strings.Count(body, `class="pcrow"`))
	}
	// 1 row of 2 cannot hold five PCs: PC-03 PC-02 stay, the others follow in
	// added rows: PC-03 PC-02 / PC-01 PC-04 / PC-05 _.
	post(admin, csrf, url.Values{"action": {"resize"}, "rows": {"1"}, "cols": {"2"}})
	body = page(admin, "/admin/new/today")
	if got := order(body); got != "PC-03 PC-02 PC-01 PC-04 PC-05" || strings.Count(body, `class="pcrow"`) != 3 || !strings.Contains(body, `name="rows" min="1" max="30" value="1"`) {
		t.Errorf("after 1x2: %s, %d rows", got, strings.Count(body, `class="pcrow"`))
	}
	// Refused changes leave the grid alone.
	for _, f := range []url.Values{
		{"action": {"resize"}, "rows": {"0"}, "cols": {"3"}},
		{"action": {"resize"}, "rows": {"x"}, "cols": {"3"}},
		{"action": {"resize"}, "rows": {"31"}, "cols": {"3"}},
		{"action": {"swap"}, "host": {"PC-99"}, "to": {"1"}},
		{"action": {"swap"}, "host": {"PC-01"}, "to": {"77"}},
		{"action": {"swap"}, "host": {"PC-01"}, "to": {"-1"}},
		{"action": {"swap"}, "host": {"PC-03"}, "to": {"0"}}, // dropped on its own cell
		{"action": {"explode"}},
	} {
		if code := post(admin, csrf, f); code != http.StatusSeeOther {
			t.Errorf("%v: %d", f, code)
		}
	}
	if got := order(page(admin, "/admin/new/today")); got != "PC-03 PC-02 PC-01 PC-04 PC-05" {
		t.Errorf("after refused changes: %s", got)
	}
	// Reset: back to 5 x 10 in name order.
	post(admin, csrf, url.Values{"action": {"reset"}})
	body = page(admin, "/admin/new/today")
	if got := order(body); got != "PC-01 PC-02 PC-03 PC-04 PC-05" || strings.Count(body, `class="pcrow"`) != 5 || strings.Contains(body, `id="pc-reset"`) {
		t.Errorf("after reset: %s", got)
	}
	// Five changes were made, each in the journal with the admin's name.
	var n int
	ts.s.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'today_layout' AND actor = 'boss'").Scan(&n)
	if n != 5 {
		t.Errorf("journal entries: %d, want 5 (2 moves, 2 sizes, 1 reset)", n)
	}
}
