package hours

import (
	"math/rand"
	"testing"
	"time"
)

// All expected values below were computed by hand from the visit times.

var almaty = mustLoc("Asia/Almaty") // UTC+5, no DST
var berlin = mustLoc("Europe/Berlin")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// at parses "2006-01-02 15:04" in loc.
func at(loc *time.Location, s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func v(key string, student int64, start, end time.Time) Visit {
	return Visit{Key: key, StudentID: student, Host: "PC-1", Start: start, End: end}
}

func mins(n int) time.Duration { return time.Duration(n) * time.Minute }

func TestSummarizeHandComputed(t *testing.T) {
	A := func(s string) time.Time { return at(almaty, s) }
	calc := Calc{Loc: almaty}
	cases := []struct {
		name    string
		visits  []Visit
		period  Period
		total   time.Duration
		days    map[string]time.Duration
		nVisits int
	}{
		{
			name:    "single visit 10:00-11:30",
			visits:  []Visit{v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 11:30"))},
			period:  calc.PeriodAt("day", A("2026-03-10 12:00")),
			total:   mins(90),
			days:    map[string]time.Duration{"2026-03-10": mins(90)},
			nVisits: 1,
		},
		{
			name:    "crosses midnight: 23:30-01:15 is 30 min + 75 min",
			visits:  []Visit{v("p1", 1, A("2026-03-10 23:30"), A("2026-03-11 01:15"))},
			period:  calc.PeriodAt("week", A("2026-03-10 12:00")),
			total:   mins(105),
			days:    map[string]time.Duration{"2026-03-10": mins(30), "2026-03-11": mins(75)},
			nVisits: 1,
		},
		{
			name:    "day view of the second day only counts 00:00-01:15",
			visits:  []Visit{v("p1", 1, A("2026-03-10 23:30"), A("2026-03-11 01:15"))},
			period:  calc.PeriodAt("day", A("2026-03-11 12:00")),
			total:   mins(75),
			days:    map[string]time.Duration{"2026-03-11": mins(75)},
			nVisits: 1,
		},
		{
			name: "overlapping visits are merged: 10:00-11:00 and 10:30-12:00 is 2 h, not 2.5 h",
			visits: []Visit{
				v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 11:00")),
				v("m2", 1, A("2026-03-10 10:30"), A("2026-03-10 12:00")),
			},
			period:  calc.PeriodAt("day", A("2026-03-10 12:00")),
			total:   mins(120),
			days:    map[string]time.Duration{"2026-03-10": mins(120)},
			nVisits: 2,
		},
		{
			name: "two separate visits add up: 09:00-09:45 and 14:10-16:00",
			visits: []Visit{
				v("p1", 1, A("2026-03-10 09:00"), A("2026-03-10 09:45")),
				v("p2", 1, A("2026-03-10 14:10"), A("2026-03-10 16:00")),
			},
			period:  calc.PeriodAt("month", A("2026-03-10 12:00")),
			total:   mins(45 + 110),
			days:    map[string]time.Duration{"2026-03-10": mins(155)},
			nVisits: 2,
		},
		{
			name: "week runs Monday to Sunday: Sunday 8 Mar is last week, Monday 9 Mar is this week",
			visits: []Visit{
				v("p1", 1, A("2026-03-08 10:00"), A("2026-03-08 11:00")),
				v("p2", 1, A("2026-03-09 10:00"), A("2026-03-09 10:30")),
				v("p3", 1, A("2026-03-15 20:00"), A("2026-03-15 21:00")),
				v("p4", 1, A("2026-03-16 08:00"), A("2026-03-16 09:00")),
			},
			period:  calc.PeriodAt("week", A("2026-03-12 12:00")),
			total:   mins(90),
			days:    map[string]time.Duration{"2026-03-09": mins(30), "2026-03-15": mins(60)},
			nVisits: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := calc.Summarize(tc.visits, tc.period.From, tc.period.To)
			r := rows[1]
			if r == nil {
				t.Fatal("no row for student 1")
			}
			if r.Total != tc.total {
				t.Errorf("total = %v, want %v", r.Total, tc.total)
			}
			if r.Visits != tc.nVisits {
				t.Errorf("visits = %d, want %d", r.Visits, tc.nVisits)
			}
			if len(r.Days) != len(tc.days) {
				t.Errorf("days = %v, want %v", r.Days, tc.days)
			}
			for d, want := range tc.days {
				if r.Days[d] != want {
					t.Errorf("day %s = %v, want %v", d, r.Days[d], want)
				}
			}
		})
	}
}

func TestSummarizeFirstArrivalLastDeparture(t *testing.T) {
	A := func(s string) time.Time { return at(almaty, s) }
	calc := Calc{Loc: almaty}
	visits := []Visit{
		v("p1", 1, A("2026-03-10 14:00"), A("2026-03-10 15:00")),
		v("p2", 1, A("2026-03-10 09:15"), A("2026-03-10 10:00")),
		v("p3", 1, A("2026-03-10 23:00"), A("2026-03-11 02:00")), // cut at midnight for the day view
	}
	p := calc.PeriodAt("day", A("2026-03-10 12:00"))
	r := calc.Summarize(visits, p.From, p.To)[1]
	if !r.FirstArrival.Equal(A("2026-03-10 09:15")) {
		t.Errorf("first arrival %v", r.FirstArrival)
	}
	if !r.LastDeparture.Equal(A("2026-03-11 00:00")) {
		t.Errorf("last departure %v", r.LastDeparture)
	}
	if r.Total != mins(60+45+60) {
		t.Errorf("total %v", r.Total)
	}
}

// Spring forward in Berlin: 2026-03-29 02:00 -> 03:00. A visit from 23:00 on
// the 28th to 04:00 local on the 29th lasts 4 real hours: 1 h + 3 h.
func TestMidnightSplitAcrossDST(t *testing.T) {
	calc := Calc{Loc: berlin}
	visits := []Visit{v("p1", 1, at(berlin, "2026-03-28 23:00"), at(berlin, "2026-03-29 04:00"))}
	p := calc.PeriodAt("week", at(berlin, "2026-03-28 12:00"))
	r := calc.Summarize(visits, p.From, p.To)[1]
	if r.Days["2026-03-28"] != time.Hour || r.Days["2026-03-29"] != 3*time.Hour || r.Total != 4*time.Hour {
		t.Errorf("days %v total %v, want 1h + 3h = 4h", r.Days, r.Total)
	}
}

func TestPolicy(t *testing.T) {
	A := func(s string) time.Time { return at(almaty, s) }
	day := Calc{Loc: almaty}.PeriodAt("day", A("2026-03-10 12:00"))
	cases := []struct {
		name   string
		policy Policy
		visits []Visit
		want   time.Duration
	}{
		{"min visit 5 min drops a 4 min visit, keeps 6 min",
			Policy{MinVisit: mins(5)},
			[]Visit{v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 10:04")), v("p2", 1, A("2026-03-10 11:00"), A("2026-03-10 11:06"))},
			mins(6)},
		{"round to 5 min: 62 min -> 60",
			Policy{Rounding: mins(5)},
			[]Visit{v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 11:02"))},
			mins(60)},
		{"round to 5 min: 63 min -> 65",
			Policy{Rounding: mins(5)},
			[]Visit{v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 11:03"))},
			mins(65)},
		{"round to 5 min: exactly 2.5 min rounds up -> 5",
			Policy{Rounding: mins(5)},
			[]Visit{v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 10:02").Add(30*time.Second))},
			mins(5)},
		{"daily cap 12 h: 13 h -> 12 h",
			Policy{DailyCap: 12 * time.Hour},
			[]Visit{v("p1", 1, A("2026-03-10 08:00"), A("2026-03-10 21:00"))},
			12 * time.Hour},
		{"no rules: exact seconds are kept",
			Policy{},
			[]Visit{v("p1", 1, A("2026-03-10 10:00"), A("2026-03-10 10:00").Add(61*time.Second))},
			61 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calc := Calc{Loc: almaty, Policy: tc.policy}
			var got time.Duration
			if r := calc.Summarize(tc.visits, day.From, day.To)[1]; r != nil {
				got = r.Total
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyCorrections(t *testing.T) {
	A := func(s string) time.Time { return at(almaty, s) }
	calc := Calc{Loc: almaty}
	day := calc.PeriodAt("day", A("2026-03-10 12:00"))
	raw := []Visit{
		v("p10", 1, A("2026-03-10 10:00"), A("2026-03-10 10:20")),
		v("p11", 1, A("2026-03-10 13:00"), A("2026-03-10 14:00")),
	}
	corr := []Correction{
		// Student forgot to scan back in after lunch; staff stretch the first visit.
		{ID: 1, Action: "edit", Target: "p10", StudentID: 1, Start: A("2026-03-10 10:00"), End: A("2026-03-10 12:00"), Reason: "was here"},
		// The afternoon scan was a friend's: delete it.
		{ID: 2, Action: "delete", Target: "p11", StudentID: 1, Reason: "spot check"},
		// Missed visit (no phone): add 15:00-16:30.
		{ID: 3, Action: "add", StudentID: 1, Host: "PC-2", Start: A("2026-03-10 15:00"), End: A("2026-03-10 16:30"), Reason: "dead battery"},
		// Then shorten the added visit to 15:00-16:00.
		{ID: 4, Action: "edit", Target: "m3", StudentID: 1, Start: A("2026-03-10 15:00"), End: A("2026-03-10 16:00"), Reason: "left early"},
	}
	all := Apply(raw, corr)
	if len(all) != 3 {
		t.Fatalf("got %d visits, want 3 (2 raw + 1 manual)", len(all))
	}
	if !all[0].Edited || !all[0].OrigEnd.Equal(A("2026-03-10 10:20")) || all[0].EndReason != "manual" {
		t.Errorf("edited visit: %+v", all[0])
	}
	if !all[1].Deleted {
		t.Errorf("p11 should be deleted")
	}
	if !all[2].Manual || all[2].Edited || all[2].Host != "PC-2" {
		t.Errorf("manual visit: %+v", all[2])
	}
	// The raw input is untouched.
	if !raw[0].End.Equal(A("2026-03-10 10:20")) || raw[1].Deleted {
		t.Errorf("Apply modified its input")
	}
	// 10:00-12:00 (120) + 15:00-16:00 (60) = 180 min.
	if r := calc.Summarize(all, day.From, day.To)[1]; r.Total != mins(180) || r.Visits != 2 {
		t.Errorf("total %v visits %d, want 3h and 2", r.Total, r.Visits)
	}
}

func TestPeriods(t *testing.T) {
	calc := Calc{Loc: almaty}
	A := func(s string) time.Time { return at(almaty, s) }
	w := calc.PeriodAt("week", A("2026-03-15 23:59")) // Sunday
	if !w.From.Equal(A("2026-03-09 00:00")) || !w.To.Equal(A("2026-03-16 00:00")) {
		t.Errorf("week %v - %v", w.From, w.To)
	}
	m := calc.PeriodAt("month", A("2026-12-31 10:00"))
	if !m.From.Equal(A("2026-12-01 00:00")) || !m.To.Equal(A("2027-01-01 00:00")) {
		t.Errorf("month %v - %v", m.From, m.To)
	}
	if p := calc.Shift(m, 1); !p.From.Equal(A("2027-01-01 00:00")) {
		t.Errorf("next month %v", p.From)
	}
	if p := calc.Shift(w, -1); !p.From.Equal(A("2026-03-02 00:00")) {
		t.Errorf("previous week %v", p.From)
	}
}

// Invariants on random data: recorded hours never exceed wall-clock time,
// a day never has more than 24 h, and merged intervals never overlap.
func TestInvariantsRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	calc := Calc{Loc: almaty, Policy: Policy{Rounding: mins(5)}}
	base := at(almaty, "2026-03-01 00:00")
	for iter := 0; iter < 300; iter++ {
		var visits []Visit
		for i := 0; i < 1+rng.Intn(12); i++ {
			s := base.Add(time.Duration(rng.Int63n(int64(30 * 24 * time.Hour))))
			e := s.Add(time.Duration(rng.Int63n(int64(30 * time.Hour))))
			visits = append(visits, v("p", 1, s, e))
		}
		p := calc.PeriodAt("month", base)
		r := calc.Summarize(visits, p.From, p.To)[1]
		if r == nil {
			continue
		}
		var union time.Duration
		var ivs []interval
		for _, x := range visits {
			ivs = append(ivs, interval{maxTime(x.Start, p.From), minTime(x.End, p.To)})
		}
		merged := merge(ivs)
		for i := 1; i < len(merged); i++ {
			if !merged[i].s.After(merged[i-1].e) {
				t.Fatalf("merged intervals overlap: %v", merged)
			}
		}
		for _, iv := range merged {
			union += iv.e.Sub(iv.s)
		}
		// Rounding can add at most half a step per day.
		if slack := time.Duration(len(r.Days)) * calc.Policy.Rounding / 2; r.Total > union+slack {
			t.Fatalf("total %v exceeds wall-clock union %v", r.Total, union)
		}
		if r.Total > p.To.Sub(p.From) {
			t.Fatalf("total %v exceeds the period", r.Total)
		}
		for d, x := range r.Days {
			if x > 24*time.Hour {
				t.Fatalf("day %s has %v", d, x)
			}
		}
	}
}
