package core

import (
	"errors"
	"strings"
	"testing"
)

// At 10:05 Alice (PC-01) and Bob (PC-02) are checked in; Carol checked in on
// PC-03 and checked out at 10:02, so she is not present. By hand:
//   - 5 points to everyone present at 10:05: Alice +5, Bob +5, Carol 0.
//   - 3 points to Carol alone: Carol 3.
//   - 2 points to everyone present at 10:06: Alice 7, Bob 7.
//   - cancelling the first award: Alice 2, Bob 2; cancelling Carol's 3: 0.
func TestPoints(t *testing.T) {
	e := newEnv(t)
	for i, pc := range []string{"PC-01", "PC-02", "PC-03"} {
		e.start(T("10:00"), pc, "s")
		if r := e.scan(T("10:01"), pc, tgAlice+int64(i)); r.Result != ScanOK {
			t.Fatalf("scan %s: %s", pc, r.Result)
		}
	}
	if _, err := e.engine.Checkout(e.ctx, T("10:02"), tgCarol, carol); err != nil {
		t.Fatal(err)
	}
	totals := func() string {
		tot, err := PointsTotals(e.ctx, e.db)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join([]string{itoa(tot[alice]), itoa(tot[bob]), itoa(tot[carol])}, " ")
	}

	n, err := AwardPresent(e.ctx, e.db, T("10:05"), 5, "  cleaned   the lab ", "boss")
	if err != nil || n != 2 {
		t.Fatalf("award to present: %d, %v", n, err)
	}
	if got := totals(); got != "5 5 0" {
		t.Errorf("after the first award: %s", got)
	}
	if err := AwardStudent(e.ctx, e.db, T("10:05"), carol, 3, "helped at the open day", "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := AwardPresent(e.ctx, e.db, T("10:06"), 2, "quiz", "boss"); err != nil {
		t.Fatal(err)
	}
	if got := totals(); got != "7 7 3" {
		t.Errorf("after three awards: %s", got)
	}
	entries, total, err := StudentPoints(e.ctx, e.db, alice)
	if err != nil || total != 7 || len(entries) != 2 || entries[0].Reason != "quiz" || entries[1].Reason != "cleaned the lab" || entries[1].Batch == 0 {
		t.Errorf("Alice's entries: %d total, %+v, %v", total, entries, err)
	}
	batches, err := Batches(e.ctx, e.db, T("00:00"), T("+1 00:00"))
	if err != nil || len(batches) != 2 || batches[0].Points != 2 || batches[1].Students != 2 || batches[1].Reason != "cleaned the lab" {
		t.Fatalf("batches: %+v %v", batches, err)
	}

	// Cancelling.
	if k, err := CancelBatch(e.ctx, e.db, T("10:10"), batches[1].Batch, "boss"); err != nil || k != 2 {
		t.Errorf("cancel batch: %d %v", k, err)
	}
	if _, err := CancelBatch(e.ctx, e.db, T("10:10"), batches[1].Batch, "boss"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("cancelling twice: %v", err)
	}
	carolEntries, _, _ := StudentPoints(e.ctx, e.db, carol)
	if st, err := CancelPoints(e.ctx, e.db, T("10:11"), carolEntries[0].ID, "boss"); err != nil || st != carol {
		t.Errorf("cancel Carol's entry: %d %v", st, err)
	}
	if got := totals(); got != "2 2 0" {
		t.Errorf("after cancelling: %s", got)
	}
	entries, total, _ = StudentPoints(e.ctx, e.db, alice)
	if total != 2 || entries[1].Cancelled.IsZero() || entries[1].CancelledBy != "boss" {
		t.Errorf("a cancelled entry is kept and marked: %d %+v", total, entries[1])
	}
	if batches, _ := Batches(e.ctx, e.db, T("00:00"), T("+1 00:00")); !batches[1].Cancelled || batches[0].Cancelled {
		t.Errorf("batch states: %+v", batches)
	}

	// Refused input.
	for _, c := range []struct {
		points int
		reason string
		want   error
	}{{0, "x", ErrBadPoints}, {-3, "x", ErrBadPoints}, {1001, "x", ErrBadPoints}, {5, "   ", ErrNoReason}} {
		if err := AwardStudent(e.ctx, e.db, T("10:20"), alice, c.points, c.reason, "boss"); !errors.Is(err, c.want) {
			t.Errorf("%d %q: %v, want %v", c.points, c.reason, err, c.want)
		}
	}
	if err := AwardStudent(e.ctx, e.db, T("10:20"), 99, 5, "x", "boss"); err == nil {
		t.Error("points for an unknown student")
	}
	// An archived student who is still checked in gets nothing.
	if err := SetArchived(e.ctx, e.db, T("10:30"), bob, true, "boss"); err != nil {
		t.Fatal(err)
	}
	if n, _ := AwardPresent(e.ctx, e.db, T("10:31"), 1, "x", "boss"); n != 1 {
		t.Errorf("present after archiving Bob: %d, want 1", n)
	}
	// Nobody checked in.
	e.agent(T("10:40"), "PC-01", KindLogout, "s", idle(0))
	if _, err := AwardPresent(e.ctx, e.db, T("10:41"), 1, "x", "boss"); !errors.Is(err, ErrNobodyHere) {
		t.Errorf("nobody present: %v", err)
	}
	var journal int
	e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action LIKE 'points%'").Scan(&journal)
	if journal != 6 { // 3 awards, 1 batch cancel, 1 entry cancel, 1 award after archiving
		t.Errorf("journal entries: %d, want 6", journal)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	if neg {
		s = "-" + s
	}
	return s
}

func TestLogins(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ in, want string }{{" @Alice_01 ", "Alice_01"}, {"", ""}, {"a.b-c", "a.b-c"}} {
		if got, err := CleanLogin(c.in); err != nil || got != c.want {
			t.Errorf("CleanLogin(%q) = %q, %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"two words", "алиса", "-x", strings.Repeat("a", 65), "a/b"} {
		if _, err := CleanLogin(bad); err == nil {
			t.Errorf("CleanLogin(%q) accepted", bad)
		}
	}
	if err := SetLogin(e.ctx, e.db, T("09:00"), alice, "AliceS", "boss"); err != nil {
		t.Fatal(err)
	}
	// Logins are unique whatever the case.
	if err := SetLogin(e.ctx, e.db, T("09:00"), bob, "alices", "boss"); err == nil || !strings.Contains(err.Error(), "Alice") {
		t.Errorf("duplicate login: %v", err)
	}
	if err := SetLogin(e.ctx, e.db, T("09:00"), alice, "", "boss"); err != nil {
		t.Errorf("clearing: %v", err)
	}
	if err := SetLogin(e.ctx, e.db, T("09:00"), bob, "alices", "boss"); err != nil {
		t.Errorf("after clearing it is free: %v", err)
	}

	// A new student can come with a login; a bad or taken one refuses the
	// student, so nobody is added half-done.
	var before int
	e.db.QueryRow("SELECT COUNT(*) FROM students").Scan(&before)
	if _, err := AddStudentLogin(e.ctx, e.db, T("09:00"), "Dana", "B", "ALICES", "boss"); err == nil || !strings.Contains(err.Error(), "Bob") {
		t.Errorf("new student with Bob's login: %v", err)
	}
	if _, err := AddStudentLogin(e.ctx, e.db, T("09:00"), "Dana", "B", "da na", "boss"); !errors.Is(err, ErrBadPlatformLogin) {
		t.Errorf("new student with a bad login: %v", err)
	}
	var after int
	e.db.QueryRow("SELECT COUNT(*) FROM students").Scan(&after)
	if after != before {
		t.Errorf("refused students were added: %d -> %d", before, after)
	}
	id, err := AddStudentLogin(e.ctx, e.db, T("09:00"), " Dana  Lee ", "B", "@dlee", "boss")
	if err != nil {
		t.Fatal(err)
	}
	var name, login, journal string
	e.db.QueryRow("SELECT name, login FROM students WHERE id = ?", id).Scan(&name, &login)
	e.db.QueryRow("SELECT details FROM audit_log WHERE action = 'add_student' ORDER BY id DESC").Scan(&journal)
	if name != "Dana Lee" || login != "dlee" || journal != `Dana Lee, login "dlee"` {
		t.Errorf("new student: %q %q, journal %q", name, login, journal)
	}
}

// The login column of a student CSV. By hand:
//   - first file: Ivanov (new, login ivan1), Alice (exists by name, gets login alice7), Dave (new, no login);
//   - second file: "IVAN1" finds Ivanov by login and renames him (the login
//     keeps its spelling, ivan1); Alice's row without a login keeps alice7;
//     Carol's row is refused: its login bob99 is Bob's, but Carol is another
//     student; Eve's login is not a login.
//   - an old import whose "login" column went into the id: found by that id.
func TestImportLogins(t *testing.T) {
	e := newEnv(t)
	if err := SetLogin(e.ctx, e.db, T("09:00"), bob, "bob99", "boss"); err != nil {
		t.Fatal(err)
	}
	res, err := ImportStudents(e.ctx, e.db, T("09:00"), strings.NewReader("name,group,login\nIvanov Ivan,A,ivan1\nAlice,A,alice7\nDave,B,\n"), "boss")
	if err != nil || res.Added != 2 || res.Updated != 1 || len(res.Warnings) != 0 {
		t.Fatalf("first import: %+v %v", res, err)
	}
	login := func(name string) string {
		var l string
		e.db.QueryRow("SELECT login FROM students WHERE name = ?", name).Scan(&l)
		return l
	}
	if login("Ivanov Ivan") != "ivan1" || login("Alice") != "alice7" || login("Dave") != "" {
		t.Errorf("logins: %q %q %q", login("Ivanov Ivan"), login("Alice"), login("Dave"))
	}
	res, _ = ImportStudents(e.ctx, e.db, T("09:00"), strings.NewReader("ФИО;Группа;Логин\nIvanov Ivan Petrovich;A;IVAN1\nAlice;A;\nCarol;A;bob99\nEve;A;bad login\n"), "boss")
	if res.Added != 0 || res.Updated != 1 || res.Unchanged != 1 || len(res.Warnings) != 2 {
		t.Errorf("second import: %+v", res)
	}
	if login("Ivanov Ivan Petrovich") != "ivan1" || login("Alice") != "alice7" || login("Carol") != "" {
		t.Errorf("after the second import: %q %q %q", login("Ivanov Ivan Petrovich"), login("Alice"), login("Carol"))
	}
	mustExec(t, e.db, "INSERT INTO students(name, grp, external_id, created_at) VALUES('Frank', 'C', 'frank5', 0)")
	res, _ = ImportStudents(e.ctx, e.db, T("09:00"), strings.NewReader("name,group,login\nFrank Old,C,frank5\n"), "boss")
	if res.Updated != 1 || res.Added != 0 || login("Frank Old") != "frank5" {
		t.Errorf("legacy id as login: %+v, login %q", res, login("Frank Old"))
	}
}
