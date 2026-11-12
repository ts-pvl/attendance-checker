// Package dash computes the analytics dashboard of the admin UI from the
// same visits as the Hours screen (hours.Load) and the same totals
// (Calc.Summarize), so the numbers on both screens always agree.
//
// Definitions, for a period of whole local days [From, To):
//   - A school day is a day of the period on which at least one student
//     (any status) was present.
//   - A student is active if they were present on one of the last
//     ActiveDays days of the period (ending today if the period runs into
//     the future) and is not archived; otherwise they have left.
//   - Attendance is days present / school days. The dashboard figure is the
//     mean over the students shown.
//   - Arrival is the start of a student's first visit of the day.
//   - Students with no time in the period are not shown.
package dash

import (
	"sort"
	"time"

	"attendance/internal/hours"
)

// ActiveDays is how many recent days decide whether a student is active.
const ActiveDays = 3

type Student struct {
	ID       int64
	Name     string
	Group    string
	Archived bool
}

type Options struct {
	Calc     hours.Calc
	From, To time.Time // local midnights; To is exclusive
	Now      time.Time
	Status   string // "" (all), "active" or "left"
}

type Day struct {
	Date     time.Time
	Hours    time.Duration // all students shown, after the daily policy
	Students int           // students shown who were present
	Weekend  bool
	Future   bool // after today
}

type StudentStat struct {
	Student
	Active     bool
	Total      time.Duration
	Days       int // days present
	Sessions   int // visits
	Attendance float64
	PerDay     time.Duration // Total / Days
	AvgArrival time.Duration // since local midnight
	HasArrival bool
	LastVisit  time.Time       // local midnight of the last day present
	DayHours   []time.Duration // per day of the period
	Cells      []int           // matrix bucket per day: 0 none … 6 for 10 h or more
}

type Dashboard struct {
	From, To   time.Time
	Days       []Day
	SchoolDays int
	Students   []StudentStat // most hours first

	Active        int // students shown who are active
	Total         time.Duration
	Sessions      int
	StudentDays   int
	AvgSession    time.Duration
	PerStudentDay time.Duration
	Attendance    float64 // mean of the students' attendance
	AvgArrival    time.Duration
	HasArrival    bool

	Top      *StudentStat
	PeakDay  *Day
	PeakHour int         // hour of the day with the most students in school; -1 if none
	Hourly   [24]float64 // average students in school during each hour, over school days
	Weekday  [7]float64  // hours per present student, Monday first
}

// Bucket maps hours on one day to a matrix cell: 0 none, 1 under 2 h,
// 2 for 2–4 h, 3 for 4–6 h, 4 for 6–8 h, 5 for 8–10 h, 6 for 10 h or more.
func Bucket(d time.Duration) int {
	switch {
	case d <= 0:
		return 0
	case d < 2*time.Hour:
		return 1
	case d >= 10*time.Hour:
		return 6
	default:
		return int(d/(2*time.Hour)) + 1
	}
}

const dayKey = "2006-01-02"

func Build(visits []hours.Visit, students map[int64]Student, o Options) *Dashboard {
	c := o.Calc
	d := &Dashboard{From: o.From, To: o.To, PeakHour: -1}
	today := c.DayStart(o.Now)
	var dayKeys []string
	for day := o.From; day.Before(o.To); day = day.AddDate(0, 0, 1) {
		day = c.DayStart(day.Add(12 * time.Hour)) // stays on local midnight across DST
		wd := day.Weekday()
		d.Days = append(d.Days, Day{Date: day, Weekend: wd == time.Saturday || wd == time.Sunday, Future: day.After(today)})
		dayKeys = append(dayKeys, day.Format(dayKey))
	}
	index := map[string]int{}
	for i, k := range dayKeys {
		index[k] = i
	}

	totals := c.Summarize(visits, o.From, o.To)

	// School days count every student, whatever the status filter.
	open := make([]bool, len(d.Days))
	for _, r := range totals {
		for k, v := range r.Days {
			if i, ok := index[k]; ok && v > 0 {
				open[i] = true
			}
		}
	}
	for _, x := range open {
		if x {
			d.SchoolDays++
		}
	}

	// The activity window: the last ActiveDays days up to the end of the
	// period or today, whichever is earlier.
	last := len(d.Days) - 1
	for last >= 0 && d.Days[last].Future {
		last--
	}

	arrivals := firstArrivals(c, visits, o.From, o.To)

	for id, r := range totals {
		st, ok := students[id]
		if !ok || r.Total <= 0 {
			continue
		}
		s := StudentStat{Student: st, Total: r.Total, Sessions: r.Visits,
			DayHours: make([]time.Duration, len(d.Days)), Cells: make([]int, len(d.Days))}
		recent := false
		for i, k := range dayKeys {
			v := r.Days[k]
			if v <= 0 {
				continue
			}
			s.DayHours[i], s.Cells[i] = v, Bucket(v)
			s.Days++
			s.LastVisit = d.Days[i].Date
			if i <= last && i > last-ActiveDays {
				recent = true
			}
		}
		s.Active = recent && !st.Archived
		if o.Status == "active" && !s.Active || o.Status == "left" && s.Active {
			continue
		}
		if d.SchoolDays > 0 {
			s.Attendance = float64(s.Days) / float64(d.SchoolDays)
		}
		if s.Days > 0 {
			s.PerDay = s.Total / time.Duration(s.Days)
		}
		if a := arrivals[id]; len(a) > 0 {
			s.AvgArrival, s.HasArrival = mean(a), true
		}
		d.Students = append(d.Students, s)
	}
	sort.Slice(d.Students, func(i, j int) bool {
		a, b := d.Students[i], d.Students[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Name < b.Name
	})

	var allArrivals []time.Duration
	var attSum float64
	var wdHours [7]time.Duration
	var wdPresent [7]int
	shown := map[int64]bool{}
	for i := range d.Students {
		s := &d.Students[i]
		shown[s.ID] = true
		d.Total += s.Total
		d.Sessions += s.Sessions
		d.StudentDays += s.Days
		attSum += s.Attendance
		if s.Active {
			d.Active++
		}
		allArrivals = append(allArrivals, arrivals[s.ID]...)
		for j, v := range s.DayHours {
			if v <= 0 {
				continue
			}
			d.Days[j].Hours += v
			d.Days[j].Students++
			wd := (int(d.Days[j].Date.Weekday()) + 6) % 7
			wdHours[wd] += v
			wdPresent[wd]++
		}
	}
	if n := len(d.Students); n > 0 {
		d.Top = &d.Students[0]
		d.Attendance = attSum / float64(n)
	}
	if d.Sessions > 0 {
		d.AvgSession = d.Total / time.Duration(d.Sessions)
	}
	if d.StudentDays > 0 {
		d.PerStudentDay = d.Total / time.Duration(d.StudentDays)
	}
	if len(allArrivals) > 0 {
		d.AvgArrival, d.HasArrival = mean(allArrivals), true
	}
	for i := range wdHours {
		if wdPresent[i] > 0 {
			d.Weekday[i] = wdHours[i].Hours() / float64(wdPresent[i])
		}
	}
	for i := range d.Days {
		if d.Days[i].Hours > 0 && (d.PeakDay == nil || d.Days[i].Hours > d.PeakDay.Hours) {
			d.PeakDay = &d.Days[i]
		}
	}

	// Load by hour of the day: minutes present in each hour, over school days.
	if d.SchoolDays > 0 {
		var minutes [24]time.Duration
		for id, spans := range c.Spans(visits, o.From, o.To) {
			if !shown[id] {
				continue
			}
			for _, sp := range spans {
				for t := sp.Start; t.Before(sp.End); {
					lt := t.In(c.Loc)
					next := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour()+1, 0, 0, 0, c.Loc)
					if next.After(sp.End) {
						next = sp.End
					}
					minutes[lt.Hour()] += next.Sub(t)
					t = next
				}
			}
		}
		for hr, m := range minutes {
			d.Hourly[hr] = m.Hours() / float64(d.SchoolDays)
			if m > 0 && (d.PeakHour < 0 || d.Hourly[hr] > d.Hourly[d.PeakHour]) {
				d.PeakHour = hr
			}
		}
	}
	return d
}

// firstArrivals returns, per student, the time of day of their first visit
// on each day of [from, to) on which a visit started.
func firstArrivals(c hours.Calc, visits []hours.Visit, from, to time.Time) map[int64][]time.Duration {
	first := map[int64]map[string]time.Time{}
	for _, v := range c.Counted(visits, from, to) {
		if v.Start.Before(from) || !v.Start.Before(to) {
			continue
		}
		k := v.Start.In(c.Loc).Format(dayKey)
		m := first[v.StudentID]
		if m == nil {
			m = map[string]time.Time{}
			first[v.StudentID] = m
		}
		if t, ok := m[k]; !ok || v.Start.Before(t) {
			m[k] = v.Start
		}
	}
	out := map[int64][]time.Duration{}
	for id, m := range first {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out[id] = append(out[id], m[k].Sub(c.DayStart(m[k])))
		}
	}
	return out
}

func mean(ds []time.Duration) time.Duration {
	var sum time.Duration
	for _, x := range ds {
		sum += x
	}
	return sum / time.Duration(len(ds))
}
