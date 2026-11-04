package core

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"attendance/internal/token"
)

func (e *env) scanToken(at time.Time, tgUser int64, tok string) string {
	e.t.Helper()
	r, err := e.checkin.Scan(e.ctx, at, tgUser, tok)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Result
}

func TestScanSecurity(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.start(T("10:00"), "PC-02", "s2")
	keepAlive := func(at time.Time) {
		e.agent(at, "PC-01", KindHeartbeat, "s1", idle(0))
		e.agent(at, "PC-02", KindHeartbeat, "s2", idle(0))
	}
	pc1 := e.hostID("PC-01")
	signer := e.checkin.Signer

	// Expired: a photo scanned 61 s after the code was issued.
	tok, _ := signer.Issue(uint16(pc1), T("10:00:00"))
	keepAlive(T("10:01:00"))
	if got := e.scanToken(T("10:01:01"), tgAlice, tok); got != ScanExpired {
		t.Errorf("61 s old token: %s", got)
	}
	// Acceptance criterion: more than two minutes old is always rejected.
	if got := e.scanToken(T("10:02:01"), tgAlice, tok); got != ScanExpired {
		t.Errorf("2 min old token: %s", got)
	}

	// Unlinked Telegram user: rejected, and the code is not burnt for others.
	tok, _ = signer.Issue(uint16(pc1), T("10:02:30"))
	keepAlive(T("10:02:30"))
	if got := e.scanToken(T("10:02:35"), tgEve, tok); got != ScanNotLinked {
		t.Errorf("unlinked user: %s", got)
	}
	if got := e.scanToken(T("10:02:40"), tgAlice, tok); got != ScanOK {
		t.Errorf("same token by a linked student after an unlinked attempt: %s", got)
	}
	// Replayed token: single use, also for a different student.
	if got := e.scanToken(T("10:02:45"), tgBob, tok); got != ScanUsed {
		t.Errorf("replayed token: %s", got)
	}

	// Token "from another host": flip a bit in the host id.
	tok, _ = signer.Issue(uint16(pc1), T("10:03:00"))
	forged := []byte(tok)
	forged[2] ^= 1
	if strings.ContainsAny(string(forged[2:3]), "+/=") || string(forged) == tok {
		t.Skip("bit flip produced an invalid base64 char; adjust test")
	}
	if got := e.scanToken(T("10:03:05"), tgBob, string(forged)); got != ScanBadToken {
		t.Errorf("forged host: %s", got)
	}
	// Signed with another key (a leaked old secret, or a guess).
	other := token.NewSigner(bytes.Repeat([]byte{9}, 32))
	tok, _ = other.Issue(uint16(pc1), T("10:03:00"))
	if got := e.scanToken(T("10:03:05"), tgBob, tok); got != ScanBadToken {
		t.Errorf("wrong key: %s", got)
	}
	// Correctly signed but for a host that is not in the hosts file.
	tok, _ = signer.Issue(999, T("10:03:00"))
	if got := e.scanToken(T("10:03:05"), tgBob, tok); got != ScanUnknownHost {
		t.Errorf("unknown host: %s", got)
	}

	// Account unlinked by admin: further scans are refused.
	if err := Unlink(e.ctx, e.db, T("10:04"), bob, "admin", "lost phone"); err != nil {
		t.Fatal(err)
	}
	keepAlive(T("10:04"))
	tok, _ = signer.Issue(uint16(pc1), T("10:04:00"))
	if got := e.scanToken(T("10:04:05"), tgBob, tok); got != ScanNotLinked {
		t.Errorf("after unlink: %s", got)
	}
	// Archived student: refused even though still linked.
	if err := SetArchived(e.ctx, e.db, T("10:05"), carol, true, "admin"); err != nil {
		t.Fatal(err)
	}
	tok, _ = signer.Issue(uint16(pc1), T("10:05:00"))
	if got := e.scanToken(T("10:05:05"), tgCarol, tok); got != ScanNotLinked {
		t.Errorf("archived: %s", got)
	}

	// Every attempt is in the raw event log.
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM events WHERE kind = 'scan'").Scan(&n)
	if n != 10 {
		t.Errorf("scan events logged: %d, want 10", n)
	}
	var auditN int
	e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'unlink'").Scan(&auditN)
	if auditN != 1 {
		t.Errorf("unlink audit entries: %d", auditN)
	}
}

func TestStaleTokenIsFlagged(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	pc1 := e.hostID("PC-01")
	old, _ := e.checkin.Board.Current(pc1, T("10:00:00"))
	e.checkin.Board.Current(pc1, T("10:00:31")) // the screen has rotated to a new code
	e.agent(T("10:00:40"), "PC-01", KindHeartbeat, "s1", idle(0))
	if got := e.scanToken(T("10:00:50"), tgAlice, old); got != ScanOK {
		t.Fatalf("old but unexpired code: %s", got)
	}
	var stale bool
	var latency int64
	e.db.QueryRow("SELECT stale_token, scan_latency_ms FROM presence").Scan(&stale, &latency)
	if !stale || latency != 50_000 {
		t.Errorf("stale=%v latency=%d, want true and 50000", stale, latency)
	}
	// Using the code on screen rotates it so the next student gets a fresh one.
	cur, _ := e.checkin.Board.Current(pc1, T("10:01:00"))
	e.scanToken(T("10:01:02"), tgBob, cur)
	if next, _ := e.checkin.Board.Current(pc1, T("10:01:03")); next == cur {
		t.Error("code on screen was not replaced after use")
	}
}

func TestEnrollmentRules(t *testing.T) {
	e := newEnv(t)
	now := T("09:00")
	mustExec(t, e.db, "INSERT INTO students(id, name, grp, created_at) VALUES(10, 'Dan', 'B', 0), (11, 'Erin', 'B', 0)")
	enroll := func(at time.Time, tg int64, code string) EnrollReply {
		t.Helper()
		r, err := Enroll(e.ctx, e.db, at, TGUser{ID: tg, Name: "x"}, code)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	code, err := GenerateCode(e.ctx, e.db, now, 10, 24*time.Hour, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if r := enroll(now, 5000, "e"+strings.Repeat("a", 24)); r.Result != EnrollInvalid {
		t.Errorf("unknown code: %s", r.Result)
	}
	// An account already linked to Alice cannot take Dan's code.
	if r := enroll(now, tgAlice, code); r.Result != EnrollTGTaken || r.Student.ID != alice {
		t.Errorf("linked account: %+v", r)
	}
	if r := enroll(now.Add(time.Minute), 5000, code); r.Result != EnrollLinked || r.Student.ID != 10 {
		t.Fatalf("first use: %+v", r)
	}
	if r := enroll(now.Add(2*time.Minute), 5000, code); r.Result != EnrollAlreadySame {
		t.Errorf("same user again: %s", r.Result)
	}
	if r := enroll(now.Add(2*time.Minute), 5001, code); r.Result != EnrollUsed {
		t.Errorf("code reuse by someone else: %s", r.Result)
	}
	// A linked student cannot get a new code without an explicit unlink.
	if _, err := GenerateCode(e.ctx, e.db, now, 10, time.Hour, "admin"); err != ErrAlreadyLinked {
		t.Errorf("code for linked student: %v", err)
	}
	// Expired and superseded codes.
	c1, _ := GenerateCode(e.ctx, e.db, now, 11, time.Hour, "admin")
	if r := enroll(now.Add(61*time.Minute), 5002, c1); r.Result != EnrollExpired {
		t.Errorf("expired code: %s", r.Result)
	}
	c2, _ := GenerateCode(e.ctx, e.db, now.Add(2*time.Hour), 11, time.Hour, "admin")
	if r := enroll(now.Add(2*time.Hour), 5002, c1); r.Result != EnrollInvalid {
		t.Errorf("revoked code: %s", r.Result)
	}
	if r := enroll(now.Add(2*time.Hour), 5002, c2); r.Result != EnrollLinked {
		t.Errorf("reissued code: %s", r.Result)
	}
	// Unlink, reissue, relink with a new phone.
	if err := Unlink(e.ctx, e.db, now.Add(3*time.Hour), 11, "admin", "new phone"); err != nil {
		t.Fatal(err)
	}
	c3, err := GenerateCode(e.ctx, e.db, now.Add(3*time.Hour), 11, time.Hour, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if r := enroll(now.Add(3*time.Hour), 5003, c3); r.Result != EnrollLinked {
		t.Errorf("relink: %s", r.Result)
	}
	st, _ := LinkedStudent(e.ctx, e.db, 5002)
	if st != nil {
		t.Errorf("old account still linked to %+v", st)
	}
}

func TestImportStudents(t *testing.T) {
	e := newEnv(t)
	csv := "\xef\xbb\xbfФИО;Группа;id\nИванов Иван;A1;s-1\n  Petrov   Petr ;A1;s-2\n;;\nAlice;B;\n"
	res, err := ImportStudents(e.ctx, e.db, T("09:00"), strings.NewReader(csv), "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Alice already exists (matched by name), group A -> B is an update.
	if res.Added != 2 || res.Updated != 1 || len(res.Warnings) != 0 {
		t.Fatalf("first import: %+v", res)
	}
	csv2 := "name,group,id\nIvanov Ivan,A2,s-1\nPetrov Petr,A1,s-2\n"
	res, _ = ImportStudents(e.ctx, e.db, T("09:00"), strings.NewReader(csv2), "admin")
	if res.Added != 0 || res.Updated != 1 || res.Unchanged != 1 {
		t.Fatalf("second import: %+v", res)
	}
	var name, grp string
	e.db.QueryRow("SELECT name, grp FROM students WHERE external_id = 's-2'").Scan(&name, &grp)
	if name != "Petrov Petr" || grp != "A1" {
		t.Errorf("whitespace not normalised: %q %q", name, grp)
	}
}
