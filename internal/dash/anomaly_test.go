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

func TestIsLate(t *testing.T) {
	h := func(hh, mm int) time.Duration { return time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute }
	at := func(s string) time.Time { return A("2026-03-10 " + s) }
	night := []struct {
		at   string
		late bool
	}{{"20:59", false}, {"21:00", true}, {"23:30", true}, {"00:10", true}, {"04:59", true}, {"05:00", false}, {"12:00", false}}
	for _, c := range night {
		if got := IsLate(at(c.at), almaty, h(21, 0), h(5, 0)); got != c.late {
			t.Errorf("21:00–05:00, %s: %v", c.at, got)
		}
	}
	// A window that does not cross midnight.
	if !IsLate(at("13:30"), almaty, h(13, 0), h(14, 0)) || IsLate(at("14:00"), almaty, h(13, 0), h(14, 0)) {
		t.Error("13:00–14:00 window")
	}
}
