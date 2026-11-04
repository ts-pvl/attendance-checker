package app

import (
	"testing"
	"time"

	"attendance/internal/config"
)

func TestDue(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Almaty")
	at := func(s string) time.Time {
		t, _ := time.ParseInLocation("2006-01-02 15:04", s, loc)
		return t
	}
	four := config.Clock{Hour: 4}
	cases := []struct {
		name        string
		now, last   string
		due, missed bool
	}{
		{"just before 04:00, ran yesterday", "2026-03-10 03:59", "2026-03-09 04:00", false, false},
		{"at 04:00, ran yesterday", "2026-03-10 04:00", "2026-03-09 04:00", true, false},
		{"04:01, already ran today", "2026-03-10 04:01", "2026-03-10 04:00", false, false},
		{"server was down 03:30-05:00: catch up", "2026-03-10 05:00", "2026-03-09 04:00", true, false},
		{"server came back at 15:00: too late, skip", "2026-03-10 15:00", "2026-03-09 04:00", false, true},
		{"just after midnight, ran yesterday morning", "2026-03-10 00:30", "2026-03-09 04:00", false, false},
	}
	for _, c := range cases {
		_, due, missed := Due(at(c.now), four, at(c.last), 2*time.Hour)
		if due != c.due || missed != c.missed {
			t.Errorf("%s: due=%v missed=%v", c.name, due, missed)
		}
	}
}
