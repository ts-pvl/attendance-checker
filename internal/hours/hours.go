// Package hours turns visits into attended hours.
//
// Rules (section 3.5 of the spec):
//   - A visit counts from the confirmed scan to the check-out time (last
//     activity). Open visits count up to the PC's last activity so far.
//   - Staff corrections are layered on top of raw visits: add, edit or
//     delete. The raw rows are never changed.
//   - Overlapping visits of one student are merged before summing, so time
//     is never counted twice.
//   - Visits are split at local midnight; each day gets the time actually
//     spent on that day. Week and month totals are sums of daily totals.
//   - Policy, applied in this order: visits shorter than MinVisit are
//     dropped; each daily total is rounded to the nearest Rounding; each
//     daily total is capped at DailyCap. Zero disables a rule.
package hours

import (
	"sort"
	"strconv"
	"time"
)

type Visit struct {
	Key       string // "p<presence id>" for scanned visits, "m<correction id>" for manual ones
	StudentID int64
	Host      string
	Start     time.Time
	End       time.Time // for an open visit: the last activity so far
	Open      bool
	EndReason string
	Manual    bool // added by staff
	Edited    bool // a scanned visit changed by staff
	Deleted   bool
	LatencyMs int64
	Stale     bool
	SessionID int64
	// Applied lists the corrections that affect this visit, oldest first.
	Applied []Correction
	// Original holds the scanned times before an edit.
	OrigStart, OrigEnd time.Time
	OrigHost           string
}

func (v Visit) Duration() time.Duration {
	if v.End.Before(v.Start) {
		return 0
	}
	return v.End.Sub(v.Start)
}

type Correction struct {
	ID        int64
	Action    string // add, edit, delete
	Target    string // visit key; empty for add
	StudentID int64
	Host      string
	Start     time.Time
	End       time.Time
	Reason    string
	Actor     string
	Created   time.Time
}

// Key returns the visit key a manual "add" correction creates.
func (c Correction) Key() string { return "m" + strconv.FormatInt(c.ID, 10) }

// Apply layers corrections (in id order) over raw visits. Deleted visits are
// kept with Deleted set so the detail view can show them; Effective drops them.
func Apply(raw []Visit, corrections []Correction) []Visit {
	byKey := make(map[string]int, len(raw))
	out := make([]Visit, len(raw))
	copy(out, raw)
	for i := range out {
		byKey[out[i].Key] = i
	}
	cs := append([]Correction(nil), corrections...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	for _, c := range cs {
		switch c.Action {
		case "add":
			byKey[c.Key()] = len(out)
			out = append(out, Visit{
				Key: c.Key(), StudentID: c.StudentID, Host: c.Host,
				Start: c.Start, End: c.End, EndReason: "manual", Manual: true,
				Applied: []Correction{c},
			})
		case "edit":
			i, ok := byKey[c.Target]
			if !ok {
				continue
			}
			v := &out[i]
			if !v.Manual && !v.Edited {
				v.OrigStart, v.OrigEnd, v.OrigHost = v.Start, v.End, v.Host
			}
			v.Start, v.End, v.Open = c.Start, c.End, false
			if c.Host != "" {
				v.Host = c.Host
			}
			v.EndReason = "manual"
			v.Edited = !v.Manual
			v.Deleted = false
			v.Applied = append(v.Applied, c)
		case "delete":
			i, ok := byKey[c.Target]
			if !ok {
				continue
			}
			out[i].Deleted = true
			out[i].Applied = append(out[i].Applied, c)
		}
	}
	return out
}

// Effective returns the visits that count: not deleted and intersecting [from, to).
func Effective(visits []Visit, from, to time.Time) []Visit {
	var out []Visit
	for _, v := range visits {
		if v.Deleted || !Intersects(v, from, to) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Intersects reports whether v overlaps [from, to). A zero-length visit
// counts if it starts inside the range.
func Intersects(v Visit, from, to time.Time) bool {
	if !v.Start.Before(to) {
		return false
	}
	return v.End.After(from) || !v.Start.Before(from)
}

type Policy struct {
	Rounding time.Duration
	MinVisit time.Duration
	DailyCap time.Duration
}

// Calc computes totals in one time zone with one policy.
type Calc struct {
	Loc    *time.Location
	Policy Policy
}

// DayStart returns local midnight of the day containing t.
func (c Calc) DayStart(t time.Time) time.Time {
	t = t.In(c.Loc)
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, c.Loc)
}

func (c Calc) nextDay(day time.Time) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, c.Loc)
}

// Row is one student's totals for a period.
type Row struct {
	StudentID     int64
	Total         time.Duration
	Visits        int
	FirstArrival  time.Time // zero if no visits
	LastDeparture time.Time
	Open          bool                     // a visit is still running
	Days          map[string]time.Duration // "2006-01-02" -> total after policy
}

// Summarize computes per-student totals for [from, to). from and to should
// be local midnights.
func (c Calc) Summarize(visits []Visit, from, to time.Time) map[int64]*Row {
	rows := map[int64]*Row{}
	byStudent := map[int64][]interval{}
	for _, v := range Effective(visits, from, to) {
		if c.Policy.MinVisit > 0 && v.Duration() < c.Policy.MinVisit {
			continue
		}
		r := rows[v.StudentID]
		if r == nil {
			r = &Row{StudentID: v.StudentID, Days: map[string]time.Duration{}}
			rows[v.StudentID] = r
		}
		r.Visits++
		r.Open = r.Open || v.Open
		arr, dep := maxTime(v.Start, from), minTime(v.End, to)
		if r.FirstArrival.IsZero() || arr.Before(r.FirstArrival) {
			r.FirstArrival = arr
		}
		if dep.After(r.LastDeparture) {
			r.LastDeparture = dep
		}
		byStudent[v.StudentID] = append(byStudent[v.StudentID], interval{v.Start, v.End})
	}
	for id, ivs := range byStudent {
		r := rows[id]
		raw := map[string]time.Duration{}
		dayLen := map[string]time.Duration{}
		for _, iv := range merge(ivs) {
			s, e := maxTime(iv.s, from), minTime(iv.e, to)
			for day := c.DayStart(s); day.Before(e); day = c.nextDay(day) {
				next := c.nextDay(day)
				seg := minTime(e, next).Sub(maxTime(s, day))
				if seg > 0 {
					k := day.Format("2006-01-02")
					raw[k] += seg
					dayLen[k] = next.Sub(day)
				}
			}
		}
		for k, d := range raw {
			d = c.applyDayPolicy(d, dayLen[k])
			r.Days[k] = d
			r.Total += d
		}
	}
	return rows
}

func (c Calc) applyDayPolicy(d, dayLen time.Duration) time.Duration {
	if p := c.Policy.Rounding; p > 0 {
		d = ((d + p/2) / p) * p
		if d > dayLen {
			d = dayLen
		}
	}
	if c.Policy.DailyCap > 0 && d > c.Policy.DailyCap {
		d = c.Policy.DailyCap
	}
	return d
}

type interval struct{ s, e time.Time }

// merge returns the union of intervals, sorted.
func merge(ivs []interval) []interval {
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].s.Before(ivs[j].s) })
	var out []interval
	for _, iv := range ivs {
		if !iv.e.After(iv.s) {
			continue
		}
		if n := len(out); n > 0 && !iv.s.After(out[n-1].e) {
			if iv.e.After(out[n-1].e) {
				out[n-1].e = iv.e
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Period is a reporting range.
type Period struct {
	Kind     string // day, week, month
	From, To time.Time
}

// PeriodAt returns the day, ISO week (Monday start) or month containing t.
func (c Calc) PeriodAt(kind string, t time.Time) Period {
	day := c.DayStart(t)
	y, m, d := day.Date()
	switch kind {
	case "week":
		off := (int(day.Weekday()) + 6) % 7 // Monday = 0
		from := time.Date(y, m, d-off, 0, 0, 0, 0, c.Loc)
		return Period{"week", from, time.Date(y, m, d-off+7, 0, 0, 0, 0, c.Loc)}
	case "month":
		return Period{"month", time.Date(y, m, 1, 0, 0, 0, 0, c.Loc), time.Date(y, m+1, 1, 0, 0, 0, 0, c.Loc)}
	default:
		return Period{"day", day, c.nextDay(day)}
	}
}

// Shift moves a period n steps forward or back.
func (c Calc) Shift(p Period, n int) Period {
	y, m, d := p.From.Date()
	switch p.Kind {
	case "week":
		return c.PeriodAt("week", time.Date(y, m, d+7*n, 12, 0, 0, 0, c.Loc))
	case "month":
		return c.PeriodAt("month", time.Date(y, m+time.Month(n), 1, 12, 0, 0, 0, c.Loc))
	default:
		return c.PeriodAt("day", time.Date(y, m, d+n, 12, 0, 0, 0, c.Loc))
	}
}
