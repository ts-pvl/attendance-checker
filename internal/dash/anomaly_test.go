package dash

import (
	"testing"
	"time"
)

// Expected values are computed by hand from the pings below.

func ping(at string, idleSec int) Ping {
	return Ping{At: A("2026-03-10 " + at), Idle: time.Duration(idleSec) * time.Second}
}

func TestActivityGap(t *testing.T) {
	start, end := A("2026-03-10 10:00"), A("2026-03-10 11:00")
	cases := []struct {
		name   string
		start  time.Time
		pings  []Ping
		judged bool
		median time.Duration
		active int
	}{
		{
			// Active moments 10:00 (check-in), 10:20, 10:39:30 (ping at 10:40, idle 30 s), 11:00 (end):
			// gaps 20m, 19m30s, 20m30s; median 20m. The long-idle pings do not count.
			"three gaps", start,
			[]Ping{ping("10:10", 600), ping("10:20", 0), ping("10:30", 600), ping("10:40", 30), ping("10:50", 600)},
			true, 20 * time.Minute, 2,
		},
		{
			// Moments 10:00, 10:05, 10:20, 10:50, 11:00: gaps 5, 15, 30, 10 → median (10+15)/2.
			"even number of gaps", start,
			[]Ping{ping("10:05", 0), ping("10:20", 0), ping("10:50", 0)},
			true, 12*time.Minute + 30*time.Second, 3,
		},
		{
			// A ping right on the 90 s edge is not active; 89 s is.
			"edge of active", start,
			[]Ping{ping("10:30", 90), ping("10:40", 89)},
			true, 30 * time.Minute, 1, // 10:00 → 10:38:31 → 11:00: gaps 38m31s, 21m29s, mean 30m
		},
		{
			// Two pings report the same moment of input (10:20): it counts once.
			// Moments 10:00, 10:20, 11:00: gaps 20m, 40m → median 30m.
			"repeated moment", start,
			[]Ping{ping("10:20", 0), ping("10:21", 60)},
			true, 30 * time.Minute, 2,
		},
		{"no pings at all: not judged", start, nil, false, 0, 0},
		{"shorter than 30 minutes: not judged", A("2026-03-10 10:31"), []Ping{ping("10:40", 0)}, false, 0, 1},
		{"pings outside the visit are ignored", start, []Ping{ping("09:59", 0), ping("11:01", 0)}, false, 0, 0},
	}
	for _, c := range cases {
		median, active, judged := ActivityGap(c.start, end, c.pings)
		if judged != c.judged || active != c.active || (judged && median != c.median) {
			t.Errorf("%s: median %v active %d judged %v; want %v %d %v", c.name, median, active, judged, c.median, c.active, c.judged)
		}
	}
}

func TestIsOldScan(t *testing.T) {
	const limit = 45 * time.Second
	for _, c := range []struct {
		name    string
		stale   bool
		latency time.Duration
		old     bool
	}{
		{"fresh code on screen", false, 5 * time.Second, false},
		{"exactly at the limit", false, 45 * time.Second, false},
		{"just over the limit", false, 45*time.Second + time.Millisecond, true},
		{"over the limit", false, 50 * time.Second, true},
		{"young code, but the screen showed a newer one", true, 10 * time.Second, true},
		{"both", true, 55 * time.Second, true},
	} {
		if got := IsOldScan(c.stale, c.latency, limit); got != c.old {
			t.Errorf("%s: %v, want %v", c.name, got, c.old)
		}
	}
}
