package dash

import (
	"math"
	"testing"
	"time"

	"attendance/internal/hours"
)

// Every expected value below was computed by hand from the visits in
// scenario(); none comes from calling the code under test.

var almaty = func() *time.Location {
	l, err := time.LoadLocation("Asia/Almaty") // UTC+5, no DST
	if err != nil {
		panic(err)
	}
	return l
}()

// A parses "2006-01-02 15:04" in Almaty. 2026-03-09 is a Monday.
func A(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, almaty)
	if err != nil {
		panic(err)
	}
	return t
}

func h(x float64) time.Duration { return time.Duration(x * float64(time.Hour)) }

const (
	alice = int64(1)
	bob   = int64(2)
	carol = int64(3)
	dave  = int64(4) // no visits: not on the dashboard
	eve   = int64(5) // archived
)

func scenario() ([]hours.Visit, map[int64]Student) {
	vis := func(key string, id int64, s, e string) hours.Visit {
		return hours.Visit{Key: key, StudentID: id, Host: "PC-01", Start: A(s), End: A(e)}
	}
	visits := []hours.Visit{
		vis("p1", alice, "2026-03-09 10:00", "2026-03-09 14:00"), // Mon 4 h
		vis("p2", alice, "2026-03-10 10:00", "2026-03-10 12:00"), // Tue 2 h
		vis("p3", alice, "2026-03-10 13:00", "2026-03-10 15:00"), // Tue 2 h
		vis("p4", alice, "2026-03-12 11:00", "2026-03-12 17:00"), // Thu 6 h
		vis("p5", bob, "2026-03-09 12:00", "2026-03-09 13:30"),   // Mon 1.5 h
		vis("p6", carol, "2026-03-11 14:00", "2026-03-11 16:00"), // Wed 2 h
		vis("p7", eve, "2026-03-12 10:00", "2026-03-12 11:00"),   // Thu 1 h
	}
	students := map[int64]Student{
		alice: {ID: alice, Name: "Alice"},
		bob:   {ID: bob, Name: "Bob"},
		carol: {ID: carol, Name: "Carol"},
		dave:  {ID: dave, Name: "Dave"},
		eve:   {ID: eve, Name: "Eve", Archived: true},
	}
	return visits, students
}

func build(t *testing.T, status string, to string) *Dashboard {
	t.Helper()
	visits, students := scenario()
	return Build(visits, students, Options{
		Calc: hours.Calc{Loc: almaty},
		From: A("2026-03-09 00:00"), To: A(to),
		Now:    A("2026-03-12 20:00"), // Thursday evening
		Status: status,
	})
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestTotals(t *testing.T) {
	d := build(t, "", "2026-03-13 00:00") // Mon..Thu
	if len(d.Days) != 4 || d.SchoolDays != 4 {
		t.Fatalf("days %d, school days %d; want 4, 4", len(d.Days), d.SchoolDays)
	}
	if d.Total != h(18.5) || d.Sessions != 7 || d.StudentDays != 6 {
		t.Errorf("total %v sessions %d student-days %d; want 18h30m, 7, 6", d.Total, d.Sessions, d.StudentDays)
	}
	// 18.5 h / 7 visits; 18.5 h / 6 student-days
	if d.AvgSession != time.Duration(float64(h(18.5))/7) || d.PerStudentDay != time.Duration(float64(h(18.5))/6) {
		t.Errorf("avg session %v, per student-day %v", d.AvgSession, d.PerStudentDay)
	}
	// mean(3/4, 1/4, 1/4, 1/4)
	if !near(d.Attendance, 0.375) {
		t.Errorf("attendance %v, want 0.375", d.Attendance)
	}
	// first arrivals 10:00 10:00 11:00 (Alice), 12:00 (Bob), 14:00 (Carol), 10:00 (Eve): mean 11:10
	if !d.HasArrival || d.AvgArrival != 11*time.Hour+10*time.Minute {
		t.Errorf("avg arrival %v", d.AvgArrival)
	}
	if len(d.Students) != 4 || d.Active != 2 {
		t.Errorf("students %d active %d; want 4 (Dave has no visits) and 2", len(d.Students), d.Active)
	}
}

func TestStudentsTableAndStatus(t *testing.T) {
	d := build(t, "", "2026-03-13 00:00")
	want := []struct {
		id       int64
		active   bool
		total    time.Duration
		days     int
		sessions int
		att      float64
		perDay   time.Duration
		arrival  time.Duration
		last     time.Time
		cells    []int
	}{
		// sorted by hours, most first; cells: 0 none, 1 <2h, 2 2-4h, 3 4-6h, 4 6-8h, 5 8-10h, 6 10h+
		{alice, true, h(14), 3, 4, 0.75, time.Duration(float64(h(14)) / 3), 10*time.Hour + 20*time.Minute, A("2026-03-12 00:00"), []int{3, 3, 0, 4}},
		{carol, true, h(2), 1, 1, 0.25, h(2), 14 * time.Hour, A("2026-03-11 00:00"), []int{0, 0, 2, 0}},
		{bob, false, h(1.5), 1, 1, 0.25, h(1.5), 12 * time.Hour, A("2026-03-09 00:00"), []int{1, 0, 0, 0}}, // not seen Tue-Thu
		{eve, false, h(1), 1, 1, 0.25, h(1), 10 * time.Hour, A("2026-03-12 00:00"), []int{0, 0, 0, 1}},     // archived
	}
	for i, w := range want {
		s := d.Students[i]
		if s.ID != w.id || s.Active != w.active || s.Total != w.total || s.Days != w.days || s.Sessions != w.sessions ||
			!near(s.Attendance, w.att) || s.PerDay != w.perDay || !s.HasArrival || s.AvgArrival != w.arrival || !s.LastVisit.Equal(w.last) {
			t.Errorf("row %d: %+v\nwant %+v", i, s, w)
		}
		for j, c := range w.cells {
			if s.Cells[j] != c {
				t.Errorf("row %d (%s) cells %v, want %v", i, s.Name, s.Cells, w.cells)
				break
			}
		}
	}
}

func TestCharts(t *testing.T) {
	d := build(t, "", "2026-03-13 00:00")
	wantHours := []time.Duration{h(5.5), h(4), h(2), h(7)}
	wantStudents := []int{2, 1, 1, 2}
	for i, day := range d.Days {
		if day.Hours != wantHours[i] || day.Students != wantStudents[i] {
			t.Errorf("day %d: %v, %d students; want %v, %d", i, day.Hours, day.Students, wantHours[i], wantStudents[i])
		}
	}
	// Average students in school per hour of the day: minutes in that hour
	// summed over the 4 school days, / (60 * 4).
	wantHourly := map[int]float64{10: 0.75, 11: 0.75, 12: 0.75, 13: 0.875, 14: 0.75, 15: 0.5, 16: 0.25}
	for hr := 0; hr < 24; hr++ {
		if !near(d.Hourly[hr], wantHourly[hr]) {
			t.Errorf("hour %d: %v, want %v", hr, d.Hourly[hr], wantHourly[hr])
		}
	}
	// Hours per present student by weekday (Monday first): Mon 5.5/2, Tue 4/1, Wed 2/1, Thu 7/2.
	wantWeekday := [7]float64{2.75, 4, 2, 3.5, 0, 0, 0}
	for i := range wantWeekday {
		if !near(d.Weekday[i], wantWeekday[i]) {
			t.Errorf("weekday %d: %v, want %v", i, d.Weekday[i], wantWeekday[i])
		}
	}
}

func TestInsights(t *testing.T) {
	d := build(t, "", "2026-03-13 00:00")
	if d.Top == nil || d.Top.ID != alice {
		t.Errorf("top: %+v", d.Top)
	}
	if d.PeakDay == nil || !d.PeakDay.Date.Equal(A("2026-03-12 00:00")) || d.PeakDay.Hours != h(7) || d.PeakDay.Students != 2 {
		t.Errorf("peak day: %+v", d.PeakDay)
	}
	if d.PeakHour != 13 {
		t.Errorf("peak hour %d, want 13", d.PeakHour)
	}
}

func TestStatusFilter(t *testing.T) {
	d := build(t, "active", "2026-03-13 00:00")
	if len(d.Students) != 2 || d.Students[0].ID != alice || d.Students[1].ID != carol {
		t.Fatalf("active: %+v", d.Students)
	}
	// Alice 14 h + Carol 2 h; school days stay 4 (they count everyone).
	if d.Total != h(16) || d.Sessions != 5 || d.SchoolDays != 4 || !near(d.Attendance, 0.5) || d.Active != 2 {
		t.Errorf("active totals: %v %d %d %v %d", d.Total, d.Sessions, d.SchoolDays, d.Attendance, d.Active)
	}
	d = build(t, "left", "2026-03-13 00:00")
	if len(d.Students) != 2 || d.Students[0].ID != bob || d.Students[1].ID != eve || d.Active != 0 {
		t.Fatalf("left: %+v", d.Students)
	}
	// Bob and Eve were in on Mon and Thu only, but the school was open 4 days.
	if d.SchoolDays != 4 || !near(d.Students[0].Attendance, 0.25) {
		t.Errorf("left: school days %d, Bob's attendance %v; want 4, 0.25", d.SchoolDays, d.Students[0].Attendance)
	}
}

// A period that runs past today: the activity window ends today, not at the
// end of the period, and future days are marked.
func TestPeriodIntoTheFuture(t *testing.T) {
	d := build(t, "", "2026-03-16 00:00") // Mon..Sun, now is Thursday
	if len(d.Days) != 7 || d.SchoolDays != 4 {
		t.Fatalf("days %d school days %d", len(d.Days), d.SchoolDays)
	}
	if !d.Days[4].Future || d.Days[3].Future || !d.Days[5].Weekend || d.Days[4].Weekend {
		t.Errorf("flags: %+v", d.Days)
	}
	if d.Active != 2 || !d.Students[1].Active {
		t.Errorf("active %d; Carol (Wed) should still be active", d.Active)
	}
	if !near(d.Students[0].Attendance, 0.75) {
		t.Errorf("attendance counts school days only: %v", d.Students[0].Attendance)
	}
	// 7 days in the period but 4 school days: the hourly load averages over 4.
	if !near(d.Hourly[13], 0.875) {
		t.Errorf("hourly load at 13:00: %v, want 0.875", d.Hourly[13])
	}
}

func TestEmpty(t *testing.T) {
	d := Build(nil, map[int64]Student{}, Options{Calc: hours.Calc{Loc: almaty},
		From: A("2026-03-09 00:00"), To: A("2026-03-13 00:00"), Now: A("2026-03-12 20:00")})
	if d.Total != 0 || d.SchoolDays != 0 || d.Top != nil || d.PeakDay != nil || d.PeakHour != -1 || d.HasArrival || d.Attendance != 0 {
		t.Errorf("empty dashboard: %+v", d)
	}
}
