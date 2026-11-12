package dash

import (
	"sort"
	"time"
)

// Anomaly rules for the new Anomalies page. The thresholds come from the
// config (anomaly_low_activity_gap, anomaly_long_visit, anomaly_late_from,
// anomaly_late_to).

// ActivePing: the agent pings every 60 s with the time since the last input,
// so a ping with less idle time than this saw activity since the one before.
const ActivePing = 90 * time.Second

// MinJudged: shorter visits are not judged for low activity.
const MinJudged = 30 * time.Minute

// Ping is one heartbeat of the PC agent with the idle time it reported.
type Ping struct {
	At   time.Time
	Idle time.Duration
}

// ActivityGap returns the median gap between moments of activity during the
// visit [start, end]: the check-in, the last input before each active ping,
// and the end (the last activity). judged is false for visits shorter than
// MinJudged or without any ping inside them (manual visits, or heartbeats
// already pruned): there is nothing to go on.
func ActivityGap(start, end time.Time, pings []Ping) (median time.Duration, active int, judged bool) {
	moments := []time.Time{start, end}
	inside := 0
	for _, p := range pings {
		if p.At.Before(start) || p.At.After(end) {
			continue
		}
		inside++
		if p.Idle < ActivePing {
			active++
			if m := p.At.Add(-p.Idle); m.After(start) && m.Before(end) {
				moments = append(moments, m)
			}
		}
	}
	if end.Sub(start) < MinJudged || inside == 0 {
		return 0, active, false
	}
	sort.Slice(moments, func(i, j int) bool { return moments[i].Before(moments[j]) })
	var gaps []time.Duration
	for i := 1; i < len(moments); i++ {
		if g := moments[i].Sub(moments[i-1]); g > 0 { // a moment reported twice counts once
			gaps = append(gaps, g)
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	n := len(gaps)
	if n%2 == 1 {
		return gaps[n/2], active, true
	}
	return (gaps[n/2-1] + gaps[n/2]) / 2, active, true
}

// IsLate reports whether t (local time of day) falls in [from, to); the
// window may cross midnight (21:00–05:00). from and to are offsets from
// local midnight.
func IsLate(t time.Time, loc *time.Location, from, to time.Duration) bool {
	lt := t.In(loc)
	tod := time.Duration(lt.Hour())*time.Hour + time.Duration(lt.Minute())*time.Minute + time.Duration(lt.Second())*time.Second
	if from <= to {
		return tod >= from && tod < to
	}
	return tod >= from || tod < to
}
