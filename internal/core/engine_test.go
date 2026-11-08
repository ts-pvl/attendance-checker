package core

import (
	"testing"
	"time"

	"attendance/internal/store"
)

// Each scenario is a scripted sequence of PC and phone events with the
// visits a human would record for it. Times are wall-clock on 2026-03-10.

func TestIdleLogoutEndsAtLastActivity(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00:00"), "PC-01", "s1")
	if r := e.scan(T("10:00:20"), "PC-01", tgAlice); r.Result != ScanOK || r.Host != "PC-01" {
		t.Fatalf("scan: %+v", r)
	}
	// Alice works until 10:29:00, then walks away. The agent logs her out
	// after 30 minutes of idle and reports idle = 30.5 min at 10:59:30.
	e.activeUntil("PC-01", "s1", T("10:01"), T("10:59"), T("10:29:00"))
	cl := e.agent(T("10:59:30"), "PC-01", KindIdleLogout, "s1", idle(1_830_000))
	if len(cl) != 1 || cl[0].Reason != ReasonIdleLogout {
		t.Fatalf("closures: %+v", cl)
	}
	// Check-out at 10:29:00 (last activity), not 10:59:30: no phantom 30 minutes.
	e.wantVisits(visit{alice, "PC-01", T("10:00:20"), T("10:29:00"), ReasonIdleLogout})
}

func TestManualLogout(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.activeUntil("PC-01", "s1", T("10:01"), T("10:04"), T("10:04"))
	e.scan(T("10:05"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:06"), T("10:59"), T("11:00"))
	e.agent(T("11:00:03"), "PC-01", KindLogout, "s1", idle(3000))
	e.wantVisits(visit{alice, "PC-01", T("10:05"), T("11:00:00"), ReasonLogout})
}

func TestPowerOffTimesOutAtLastHeartbeatMinusIdle(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:39"), T("10:40"))
	e.agent(T("10:40:30"), "PC-01", KindHeartbeat, "s1", idle(30_000)) // last activity 10:40:00
	// Power cut: nothing more arrives.
	if cl := e.sweep(T("10:43:29")); len(cl) != 0 {
		t.Fatalf("closed after 2m59s of silence: %+v", cl)
	}
	e.wantVisits(visit{alice, "PC-01", T("10:01"), time.Time{}, ""})
	if cl := e.sweep(T("10:43:31")); len(cl) != 1 || cl[0].Reason != ReasonTimeout {
		t.Fatalf("timeout closures: %+v", cl)
	}
	e.wantVisits(visit{alice, "PC-01", T("10:01"), T("10:40:00"), ReasonTimeout})
	// Sweeping again changes nothing.
	if cl := e.sweep(T("11:00")); len(cl) != 0 {
		t.Fatalf("second sweep closed again: %+v", cl)
	}
}

func TestDoubleScanSamePC(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	if r := e.scan(T("10:02"), "PC-01", tgAlice); r.Result != ScanAlready {
		t.Fatalf("second scan: %s", r.Result)
	}
	e.wantVisits(visit{alice, "PC-01", T("10:01"), time.Time{}, ""})
}

func TestNewStudentReplacesPreviousOnSamePC(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:30"), T("10:30"))
	r := e.scan(T("10:31"), "PC-01", tgBob)
	if r.Result != ScanOK || r.Moved != nil {
		t.Fatalf("scan: %+v", r)
	}
	// Alice's visit ends at the PC's last activity (10:30), not at Bob's scan.
	if len(r.Displaced) != 1 || r.Displaced[0].StudentID != alice || r.Displaced[0].Host != "PC-01" ||
		!r.Displaced[0].Start.Equal(T("10:01")) || !r.Displaced[0].End.Equal(T("10:30")) || r.Displaced[0].Reason != ReasonTakenOver {
		t.Fatalf("displaced: %+v", r.Displaced)
	}
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:30"), ReasonTakenOver},
		visit{bob, "PC-01", T("10:31"), time.Time{}, ""},
	)
	// The login goes on; its end closes only Bob's visit.
	e.activeUntil("PC-01", "s1", T("10:32"), T("11:29"), T("11:00"))
	cl := e.agent(T("11:30"), "PC-01", KindIdleLogout, "s1", idle(1_800_000))
	if len(cl) != 1 || cl[0].StudentID != bob {
		t.Fatalf("closures at logout: %+v", cl)
	}
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:30"), ReasonTakenOver},
		visit{bob, "PC-01", T("10:31"), T("11:00"), ReasonIdleLogout},
	)
}

func TestPreviousStudentCanTakeThePCBack(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:10"), T("10:10"))
	e.scan(T("10:11"), "PC-01", tgBob)
	e.activeUntil("PC-01", "s1", T("10:12"), T("10:20"), T("10:20"))
	e.scan(T("10:21"), "PC-01", tgAlice)
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:10"), ReasonTakenOver},
		visit{bob, "PC-01", T("10:11"), T("10:20"), ReasonTakenOver},
		visit{alice, "PC-01", T("10:21"), time.Time{}, ""},
	)
}

func TestScanClosesOwnOtherPCAndPreviousStudent(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.start(T("10:00"), "PC-02", "s2")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.scan(T("10:02"), "PC-02", tgBob)
	for at := T("10:03"); !at.After(T("10:15")); at = at.Add(time.Minute) {
		e.activeUntil("PC-01", "s1", at, at, T("10:14"))
		e.activeUntil("PC-02", "s2", at, at, T("10:15"))
	}
	r := e.scan(T("10:16"), "PC-01", tgBob)
	if r.Moved == nil || r.Moved.Host != "PC-02" || !r.Moved.End.Equal(T("10:15")) {
		t.Fatalf("moved: %+v", r.Moved)
	}
	if len(r.Displaced) != 1 || r.Displaced[0].StudentID != alice {
		t.Fatalf("displaced: %+v", r.Displaced)
	}
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:14"), ReasonTakenOver},
		visit{bob, "PC-02", T("10:02"), T("10:15"), ReasonMoved},
		visit{bob, "PC-01", T("10:16"), time.Time{}, ""},
	)
}

// Visits opened by the earlier pair-programming rule may still be open when
// the server is upgraded: a new scan closes all of them.
func TestScanClosesEveryOpenVisitOfThatSession(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	if _, err := e.db.Exec(`INSERT INTO presence(id, student_id, host, session_id, confirmed_at, scan_latency_ms, stale_token)
		SELECT 1000000, ?, host, session_id, ?, 0, 0 FROM presence WHERE student_id = ?`, bob, store.Ms(T("10:02")), alice); err != nil {
		t.Fatal(err)
	}
	e.activeUntil("PC-01", "s1", T("10:03"), T("10:10"), T("10:10"))
	if r := e.scan(T("10:11"), "PC-01", tgCarol); len(r.Displaced) != 2 {
		t.Fatalf("displaced: %+v", r.Displaced)
	}
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:10"), ReasonTakenOver},
		visit{carol, "PC-01", T("10:11"), time.Time{}, ""},
		visit{bob, "PC-01", T("10:02"), T("10:10"), ReasonTakenOver},
	)
}

func TestStudentSwitchesPC(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:30"), T("10:30"))
	e.agent(T("10:30:30"), "PC-01", KindHeartbeat, "s1", idle(30_000)) // last active on PC-01 at 10:30:00
	e.start(T("10:31"), "PC-02", "s2")
	r := e.scan(T("10:32"), "PC-02", tgAlice)
	if r.Result != ScanOK || r.Moved == nil || r.Moved.Host != "PC-01" || !r.Moved.End.Equal(T("10:30")) {
		t.Fatalf("scan on second PC: %+v moved=%+v", r, r.Moved)
	}
	e.activeUntil("PC-02", "s2", T("10:33"), T("11:31"), T("11:32"))
	e.agent(T("11:32"), "PC-02", KindLogout, "s2", idle(0))
	// PC-01 stays logged in; its later idle logout must not touch Alice's closed visit.
	e.activeUntil("PC-01", "s1", T("10:31"), T("11:00"), T("10:30"))
	e.agent(T("11:00:10"), "PC-01", KindIdleLogout, "s1", idle(1_810_000))
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:30"), ReasonMoved},
		visit{alice, "PC-02", T("10:32"), T("11:32"), ReasonLogout},
	)
}

func TestRebootMidSession(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:20"), T("10:20"))
	// The PC reboots; a new login reports within the 3-minute timeout.
	cl := e.agent(T("10:21:30"), "PC-01", KindSessionStart, "s2", noIdle)
	if len(cl) != 1 || cl[0].Reason != ReasonReplaced {
		t.Fatalf("closures: %+v", cl)
	}
	e.wantVisits(visit{alice, "PC-01", T("10:01"), T("10:20"), ReasonReplaced})
	if r := e.scan(T("10:22"), "PC-01", tgAlice); r.Result != ScanOK {
		t.Fatalf("rescan after reboot: %s", r.Result)
	}
}

func TestAgentRestartKeepsSession(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:09"), T("10:09"))
	if cl := e.agent(T("10:10"), "PC-01", KindSessionStart, "s1", noIdle); len(cl) != 0 {
		t.Fatalf("agent restart closed visits: %+v", cl)
	}
	e.activeUntil("PC-01", "s1", T("10:11"), T("10:40"), T("10:40"))
	e.agent(T("10:41"), "PC-01", KindLogout, "s1", idle(60_000))
	e.wantVisits(visit{alice, "PC-01", T("10:01"), T("10:40"), ReasonLogout})
}

func TestNetworkBlipLongerThanTimeout(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:05"), T("10:05"))
	// 4 minutes without heartbeats, then they resume. The rule is the same
	// whether or not the periodic sweep ran in between.
	cl := e.agent(T("10:09"), "PC-01", KindHeartbeat, "s1", idle(0))
	if len(cl) != 1 || cl[0].Reason != ReasonTimeout {
		t.Fatalf("closures: %+v", cl)
	}
	e.wantVisits(visit{alice, "PC-01", T("10:01"), T("10:05"), ReasonTimeout})
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE host = 'PC-01' AND ended_at IS NULL AND implicit = 1").Scan(&n)
	if n != 1 {
		t.Fatalf("want a new implicit session after the blip, got %d", n)
	}
}

func TestHeartbeatWithoutIdleKeepsSessionButNotActivity(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.agent(T("10:02"), "PC-01", KindHeartbeat, "s1", idle(0))
	e.agent(T("10:04"), "PC-01", KindHeartbeat, "s1", noIdle)
	e.agent(T("10:06"), "PC-01", KindHeartbeat, "s1", noIdle)
	if cl := e.sweep(T("10:08")); len(cl) != 0 {
		t.Fatalf("idle-less heartbeats should keep the session alive: %+v", cl)
	}
	e.agent(T("10:07"), "PC-01", KindLogout, "s1", noIdle)
	e.wantVisits(visit{alice, "PC-01", T("10:01"), T("10:02"), ReasonLogout})
}

func TestNightlyCleanupAndMidnight(t *testing.T) {
	e := newEnv(t)
	e.start(T("23:30"), "PC-01", "s1")
	e.activeUntil("PC-01", "s1", T("23:31"), T("23:39"), T("23:39"))
	e.scan(T("23:40"), "PC-01", tgAlice)
	e.activeUntil("PC-01", "s1", T("23:41"), T("+1 03:59"), T("+1 03:59"))
	e.agent(T("+1 03:59:30"), "PC-01", KindHeartbeat, "s1", idle(0))
	cl, err := e.engine.Cleanup(e.ctx, T("+1 04:00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cl) != 1 || cl[0].Reason != ReasonAutoClosed {
		t.Fatalf("cleanup closures: %+v", cl)
	}
	// The PC session survives; Alice, still working, scans again.
	e.agent(T("+1 04:00:30"), "PC-01", KindHeartbeat, "s1", idle(0))
	if r := e.scan(T("+1 04:01"), "PC-01", tgAlice); r.Result != ScanOK {
		t.Fatalf("rescan: %s", r.Result)
	}
	e.wantVisits(
		visit{alice, "PC-01", T("23:40"), T("+1 03:59:30"), ReasonAutoClosed},
		visit{alice, "PC-01", T("+1 04:01"), time.Time{}, ""},
	)
	// A second cleanup run closes only what is open now.
	cl, _ = e.engine.Cleanup(e.ctx, T("+1 04:02"))
	if len(cl) != 1 || !cl[0].Start.Equal(T("+1 04:01")) {
		t.Fatalf("second cleanup: %+v", cl)
	}
	var open int
	e.db.QueryRow("SELECT COUNT(*) FROM presence WHERE ended_at IS NULL").Scan(&open)
	if open != 0 {
		t.Fatalf("%d visits still open after cleanup", open)
	}
}

func TestCheckoutCommand(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.start(T("10:00"), "PC-02", "s2")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.scan(T("10:01"), "PC-02", tgBob)
	for m := T("10:02"); !m.After(T("10:29")); m = m.Add(time.Minute) {
		e.activeUntil("PC-01", "s1", m, m, m)
		e.activeUntil("PC-02", "s2", m, m, T("10:20"))
	}
	// Alice is at the PC (active 50 s ago) and checks out: exact time.
	e.agent(T("10:30:00"), "PC-01", KindHeartbeat, "s1", idle(10_000))
	cl, err := e.engine.Checkout(e.ctx, T("10:30:40"), tgAlice, alice)
	if err != nil || cl == nil || !cl.End.Equal(T("10:30:40")) {
		t.Fatalf("alice checkout: %+v %v", cl, err)
	}
	// Bob left PC-02 at 10:20 and sends /checkout from the bus at 10:31:
	// he gets his last activity, not the bus time.
	e.agent(T("10:30:00"), "PC-02", KindHeartbeat, "s2", idle(600_000))
	cl, _ = e.engine.Checkout(e.ctx, T("10:31"), tgBob, bob)
	if cl == nil || !cl.End.Equal(T("10:20")) {
		t.Fatalf("bob checkout: %+v", cl)
	}
	if cl, _ := e.engine.Checkout(e.ctx, T("10:32"), tgBob, bob); cl != nil {
		t.Fatalf("second checkout closed something: %+v", cl)
	}
	e.wantVisits(
		visit{alice, "PC-01", T("10:01"), T("10:30:40"), ReasonCheckout},
		visit{bob, "PC-02", T("10:01"), T("10:20"), ReasonCheckout},
	)
}

func TestClosingTwiceChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.agent(T("10:30"), "PC-01", KindHeartbeat, "s1", idle(0))
	e.agent(T("10:31"), "PC-01", KindIdleLogout, "s1", idle(60_000))
	before := e.snapshot()
	// The logout trap reports too, and the idle logout is retried.
	if cl := e.agent(T("10:31:01"), "PC-01", KindLogout, "s1", idle(0)); len(cl) != 0 {
		t.Fatalf("second close: %+v", cl)
	}
	e.agent(T("10:31:02"), "PC-01", KindIdleLogout, "s1", idle(0))
	e.sweep(T("11:00"))
	if after := e.snapshot(); after != before {
		t.Fatalf("state changed:\n%s\nvs\n%s", before, after)
	}
}

func TestScanNeedsLiveSession(t *testing.T) {
	e := newEnv(t)
	if r := e.scan(T("10:00"), "PC-01", tgAlice); r.Result != ScanNoSession {
		t.Fatalf("no session: %s", r.Result)
	}
	e.start(T("10:00"), "PC-01", "s1")
	e.agent(T("10:01"), "PC-01", KindHeartbeat, "s1", idle(0))
	// Silent for more than 3 minutes: the session is dead even before the sweep runs.
	if r := e.scan(T("10:04:30"), "PC-01", tgAlice); r.Result != ScanNoSession {
		t.Fatalf("dead session: %s", r.Result)
	}
	e.wantVisits()
}

// Replaying the stored events must rebuild exactly the same state.
func TestReplayReproducesState(t *testing.T) {
	e := newEnv(t)
	e.start(T("10:00"), "PC-01", "s1")
	e.scan(T("10:01"), "PC-01", tgAlice)
	e.scan(T("10:01:30"), "PC-01", tgBob)
	e.scan(T("10:01:40"), "PC-01", tgEve) // rejected: not linked
	e.activeUntil("PC-01", "s1", T("10:02"), T("10:30"), T("10:30"))
	e.start(T("10:31"), "PC-02", "s2")
	e.scan(T("10:32"), "PC-02", tgAlice)
	e.agent(T("10:33"), "PC-02", KindHeartbeat, "s2", noIdle)
	e.engine.Checkout(e.ctx, T("10:34"), tgBob, bob)
	e.agent(T("10:40"), "PC-02", KindHeartbeat, "s2", idle(1000)) // after a gap: new implicit session
	e.agent(T("10:41"), "PC-03", KindHeartbeat, "s3", idle(5000)) // missed session_start
	e.scan(T("10:42"), "PC-03", tgCarol)
	e.engine.Cleanup(e.ctx, T("10:50"))
	e.sweep(T("11:00"))
	before := e.snapshot()

	n, err := e.engine.Replay(e.ctx, T("11:00"))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("replayed nothing")
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("replay differs:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
