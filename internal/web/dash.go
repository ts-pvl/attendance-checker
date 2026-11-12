package web

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"attendance/internal/dash"
	"attendance/internal/hours"
	"attendance/internal/i18n"
)

// The analytics dashboard ("new UI"). It is read-only and sits next to the
// existing admin pages; the switch in both headers moves between them and
// remembers the choice in a cookie.

const (
	uiCookie        = "att_ui"
	dashDefaultDays = 28
	dashMaxDays     = 366
)

// home is where "/admin/" and a fresh login go: the UI used last.
func (s *Server) home(r *http.Request) string {
	if c, err := r.Cookie(uiCookie); err == nil && c.Value == "new" {
		return "/admin/dashboard"
	}
	return "/admin/today"
}

func (s *Server) handleUISwitch(w http.ResponseWriter, r *http.Request) {
	which, dest := "old", "/admin/today"
	if r.PathValue("which") == "new" {
		which, dest = "new", "/admin/dashboard"
	}
	http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: which, Path: "/admin", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// dashQuery reads from / to (inclusive, YYYY-MM-DD) and status; the default
// is the last four weeks up to today.
func (s *Server) dashQuery(r *http.Request) (from, to time.Time, status string) {
	q := r.URL.Query()
	loc := s.Cfg.Timezone
	today := s.Calc.DayStart(s.Now())
	parse := func(k string) (time.Time, bool) {
		t, err := time.ParseInLocation("2006-01-02", q.Get(k), loc)
		return t, err == nil
	}
	to, okTo := parse("to")
	if !okTo {
		to = today
	}
	from, okFrom := parse("from")
	if !okFrom {
		from = to.AddDate(0, 0, -(dashDefaultDays - 1))
	}
	if to.Before(from) {
		from, to = to, from
	}
	if to.Sub(from) > dashMaxDays*24*time.Hour {
		from = to.AddDate(0, 0, -(dashMaxDays - 1))
	}
	switch status = q.Get("status"); status {
	case "active", "left":
	default:
		status = ""
	}
	return from, to, status
}

func (s *Server) dashboard(r *http.Request) (*dash.Dashboard, url.Values, error) {
	ctx := r.Context()
	from, to, status := s.dashQuery(r)
	end := to.AddDate(0, 0, 1) // exclusive
	end = s.Calc.DayStart(end.Add(12 * time.Hour))
	visits, err := hours.Load(ctx, s.DB, from, end, 0)
	if err != nil {
		return nil, nil, err
	}
	all, err := s.loadStudents(ctx)
	if err != nil {
		return nil, nil, err
	}
	students := make(map[int64]dash.Student, len(all))
	for id, st := range all {
		students[id] = dash.Student{ID: st.ID, Name: st.Name, Group: st.Group, Archived: st.Archived}
	}
	d := dash.Build(visits, students, dash.Options{Calc: s.Calc, From: from, To: end, Now: s.Now(), Status: status})
	q := url.Values{}
	q.Set("from", from.Format("2006-01-02"))
	q.Set("to", to.Format("2006-01-02"))
	if status != "" {
		q.Set("status", status)
	}
	return d, q, nil
}

type kpi struct{ Label, Value, Unit, Sub string }

type dashRow struct {
	ID                 int64
	Name, Group        string
	Active             bool
	Hours, PerDay, Att string
	Arrival, Last      string
	HoursV, PerDayV    float64
	AttV               float64
	BarW               float64 // 0..100, relative to the most hours
	Days, Sessions     int
	ArrivalV           int // minutes since midnight, -1 if none
	LastV              string
}

type dashPage struct {
	Brand           string
	From, To        string
	Status          string
	Period, Updated string
	Empty           bool
	KPIs            []kpi
	Insights        []template.HTML
	HoursChart      chart
	StudentsChart   chart
	HourlyChart     chart
	WeekdayChart    chart
	Matrix          matrix
	Rows            []dashRow
	Query           string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	d, q, err := s.dashboard(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "dash", s.dashPage(d, q))
}

func (s *Server) dashPage(d *dash.Dashboard, q url.Values) *dashPage {
	lang, loc := s.Cfg.Lang, s.Cfg.Timezone
	num := func(x float64, dec int) string { return i18n.Num(lang, x, dec) }
	clock := func(d time.Duration) string {
		m := int(d / time.Minute)
		return fmt.Sprintf("%02d:%02d", m/60%24, m%60)
	}
	day := func(t time.Time, year bool) string { return i18n.Day(lang, t.In(loc), year) }
	lastDay := d.To.AddDate(0, 0, -1)

	p := &dashPage{
		Brand:   s.t("dash.analytics"),
		From:    d.From.Format("2006-01-02"),
		To:      lastDay.Format("2006-01-02"),
		Status:  q.Get("status"),
		Period:  day(d.From, true) + " — " + day(lastDay, true),
		Updated: s.t("dash.updated", day(s.Now(), true)+", "+s.Now().In(loc).Format("15:04")),
		Empty:   len(d.Students) == 0,
		Query:   q.Encode(),
	}
	if s.Cfg.SchoolName != "" {
		p.Brand = s.Cfg.SchoolName + " - " + p.Brand
	}

	dash1 := "—"
	val := func(ok bool, v string) string {
		if !ok {
			return dash1
		}
		return v
	}
	n := len(d.Students)
	p.KPIs = []kpi{
		{s.t("dash.k_students"), strconv.Itoa(n), "", s.t("dash.k_students_sub", d.Active)},
		{s.t("dash.k_hours"), num(d.Total.Hours(), 0), "", i18n.Plural(lang, "dash.school_days", d.SchoolDays)},
		{s.t("dash.k_sessions"), strconv.Itoa(d.Sessions), "", s.t("dash.k_sessions_sub", val(d.Sessions > 0, num(d.AvgSession.Hours(), 1)))},
		{s.t("dash.k_per_day"), val(d.StudentDays > 0, num(d.PerStudentDay.Hours(), 1)), s.t("dash.h"), s.t("dash.k_per_day_sub")},
		{s.t("dash.k_att"), val(n > 0, num(math.Round(d.Attendance*100), 0)), "%", s.t("dash.k_att_sub")},
		{s.t("dash.k_arrival"), val(d.HasArrival, clock(d.AvgArrival)), "", s.t("dash.k_arrival_sub")},
	}

	if n > 0 {
		pct := int(math.Round(float64(d.Active) * 100 / float64(n)))
		p.Insights = append(p.Insights, rich(s.t("dash.i_retention"), s.t("dash.i_retention_v", d.Active, n, pct), s.t("dash.i_retention_t", dash.ActiveDays)))
		p.Insights = append(p.Insights, rich(s.t("dash.i_top"), d.Top.Name, s.t("dash.i_top_t", num(d.Top.Total.Hours(), 0), d.Top.Days)))
	}
	if d.PeakDay != nil && d.PeakHour >= 0 {
		peak := rich(s.t("dash.i_peak"), day(d.PeakDay.Date, false),
			s.t("dash.i_peak_t", num(d.PeakDay.Hours.Hours(), 0), i18n.Plural(lang, "dash.n_students", d.PeakDay.Students)))
		p.Insights = append(p.Insights, peak+template.HTML(" <strong>"+fmt.Sprintf("%02d:00–%02d:00", d.PeakHour, (d.PeakHour+1)%24)+"</strong>."))
	}

	// Charts
	var hrs, studs []float64
	var labels, hrsTitles, studTitles []string
	var light []bool
	for _, dd := range d.Days {
		hrs = append(hrs, dd.Hours.Hours())
		studs = append(studs, float64(dd.Students))
		labels = append(labels, day(dd.Date, false))
		light = append(light, dd.Weekend)
		hrsTitles = append(hrsTitles, s.t("dash.bar", day(dd.Date, false), num(dd.Hours.Hours(), 1)+" "+s.t("dash.h")))
		studTitles = append(studTitles, s.t("dash.bar", day(dd.Date, false), i18n.Plural(lang, "dash.n_students", dd.Students)))
	}
	p.HoursChart = barChart(lang, 760, 270, hrs, labels, light, hrsTitles, false)
	p.StudentsChart = lineChart(lang, 380, 270, studs, labels, studTitles)

	// Show the hours with a noticeable load (a visit running past midnight
	// should not stretch the chart over the whole day).
	peakLoad := 0.0
	for _, v := range d.Hourly {
		peakLoad = math.Max(peakLoad, v)
	}
	lo, hi := 23, 0
	for h, v := range d.Hourly {
		if v > 0 && v >= peakLoad*0.03 {
			lo, hi = min(lo, h), max(hi, h)
		}
	}
	if lo > hi {
		lo, hi = 9, 21
	}
	var hv []float64
	var hl, ht []string
	for h := lo; h <= hi; h++ {
		hv = append(hv, d.Hourly[h])
		hl = append(hl, fmt.Sprintf("%02d:00", h))
		ht = append(ht, s.t("dash.bar", fmt.Sprintf("%02d:00–%02d:00", h, (h+1)%24), num(d.Hourly[h], 1)))
	}
	p.HourlyChart = barChart(lang, 570, 270, hv, hl, nil, ht, false)

	var wv []float64
	var wl, wt []string
	for i := 0; i < 7; i++ {
		name := s.t(fmt.Sprintf("wd.%d", (i+1)%7))
		wv = append(wv, d.Weekday[i])
		wl = append(wl, name)
		wt = append(wt, s.t("dash.bar", name, num(d.Weekday[i], 1)+" "+s.t("dash.h")))
	}
	p.WeekdayChart = barChart(lang, 570, 270, wv, wl, nil, wt, false)

	p.Matrix = s.buildMatrix(d)

	maxH := 0.0
	if n > 0 {
		maxH = d.Students[0].Total.Hours()
	}
	for _, st := range d.Students {
		row := dashRow{
			ID: st.ID, Name: st.Name, Group: st.Group, Active: st.Active,
			HoursV: st.Total.Hours(), Hours: num(st.Total.Hours(), 1),
			Days: st.Days, Sessions: st.Sessions,
			AttV: st.Attendance, Att: num(math.Round(st.Attendance*100), 0) + "%",
			PerDayV: st.PerDay.Hours(), PerDay: num(st.PerDay.Hours(), 1),
			ArrivalV: -1, Arrival: dash1, Last: day(st.LastVisit, false), LastV: st.LastVisit.Format("2006-01-02"),
		}
		if maxH > 0 {
			row.BarW = 100 * st.Total.Hours() / maxH
		}
		if st.HasArrival {
			row.ArrivalV, row.Arrival = int(st.AvgArrival/time.Minute), clock(st.AvgArrival)
		}
		p.Rows = append(p.Rows, row)
	}
	return p
}

// rich renders "label <strong>value</strong> text" with everything escaped.
func rich(label, value, text string) template.HTML {
	e := template.HTMLEscapeString
	sep := " "
	if strings.HasPrefix(text, ":") {
		sep = ""
	}
	return template.HTML(e(label) + " <strong>" + e(value) + "</strong>" + sep + e(text))
}

// ---------- charts (server-side SVG; the CSP allows no inline styles) ----------

type chart struct {
	W, H           float64
	X0, X1, Y0, Y1 float64
	Ticks          []tick
	Bars           []bar
	Labels         []xlabel
	Line, Area     string
	Dots           []dot
}

type tick struct {
	Y     float64
	Label string
}

type bar struct {
	X, Y, W, H float64
	Light      bool
	Title      string
}

type xlabel struct {
	X    float64
	Text string
}

type dot struct {
	X, Y  float64
	Title string
}

// niceScale returns a round top value and step for about five gridlines.
func niceScale(maxV float64, integer bool) (top, step float64) {
	if maxV <= 0 {
		return 1, 0.25
	}
	raw := maxV / 5
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		step = m * mag
		if step*5 >= maxV {
			break
		}
	}
	if integer && step < 1 {
		step = 1
	}
	return math.Ceil(maxV/step-1e-9) * step, step
}

func newChart(lang string, w, h float64, vals []float64, integer bool) (chart, float64) {
	c := chart{W: w, H: h, X0: 38, X1: w - 8, Y0: 12, Y1: h - 26}
	maxV := 0.0
	for _, v := range vals {
		maxV = math.Max(maxV, v)
	}
	top, step := niceScale(maxV, integer)
	dec := 0
	if step < 1 {
		dec = 1
	}
	if step < 0.1 {
		dec = 2
	}
	for v := 0.0; v <= top+step/2; v += step {
		c.Ticks = append(c.Ticks, tick{Y: c.y(v, top), Label: i18n.Num(lang, v, dec)})
	}
	return c, top
}

func (c chart) y(v, top float64) float64 { return c.Y1 - v/top*(c.Y1-c.Y0) }

func (c *chart) xLabels(labels []string) {
	n := len(labels)
	if n == 0 {
		return
	}
	slot := (c.X1 - c.X0) / float64(n)
	every := int(math.Ceil(float64(n) * 46 / (c.X1 - c.X0)))
	every = max(every, 1)
	for i := 0; i < n; i += every {
		c.Labels = append(c.Labels, xlabel{X: c.X0 + slot*(float64(i)+0.5), Text: labels[i]})
	}
}

func barChart(lang string, w, h float64, vals []float64, labels []string, light []bool, titles []string, integer bool) chart {
	c, top := newChart(lang, w, h, vals, integer)
	n := len(vals)
	if n == 0 {
		return c
	}
	slot := (c.X1 - c.X0) / float64(n)
	bw := math.Min(slot*0.72, 56)
	for i, v := range vals {
		y := c.y(v, top)
		b := bar{X: c.X0 + slot*float64(i) + (slot-bw)/2, Y: y, W: bw, H: c.Y1 - y, Title: titles[i]}
		if light != nil {
			b.Light = light[i]
		}
		c.Bars = append(c.Bars, b)
	}
	c.xLabels(labels)
	return c
}

// lineChart draws a smooth line (Catmull-Rom as cubic Béziers) with the area
// under it filled; the curve is kept inside the plot.
func lineChart(lang string, w, h float64, vals []float64, labels []string, titles []string) chart {
	c, top := newChart(lang, w, h, vals, true)
	n := len(vals)
	if n == 0 {
		return c
	}
	slot := (c.X1 - c.X0) / float64(n)
	type pt struct{ x, y float64 }
	pts := make([]pt, n)
	for i, v := range vals {
		pts[i] = pt{c.X0 + slot*(float64(i)+0.5), c.y(v, top)}
		c.Dots = append(c.Dots, dot{X: pts[i].x, Y: pts[i].y, Title: titles[i]})
	}
	clampY := func(y float64) float64 { return math.Max(c.Y0, math.Min(c.Y1, y)) }
	var b strings.Builder
	fmt.Fprintf(&b, "M%.1f,%.1f", pts[0].x, pts[0].y)
	for i := 0; i < n-1; i++ {
		p0, p1, p2, p3 := pts[max(i-1, 0)], pts[i], pts[i+1], pts[min(i+2, n-1)]
		c1 := pt{p1.x + (p2.x-p0.x)/6, clampY(p1.y + (p2.y-p0.y)/6)}
		c2 := pt{p2.x - (p3.x-p1.x)/6, clampY(p2.y - (p3.y-p1.y)/6)}
		fmt.Fprintf(&b, " C%.1f,%.1f %.1f,%.1f %.1f,%.1f", c1.x, c1.y, c2.x, c2.y, p2.x, p2.y)
	}
	c.Line = b.String()
	c.Area = fmt.Sprintf("%s L%.1f,%.1f L%.1f,%.1f Z", c.Line, pts[n-1].x, c.Y1, pts[0].x, c.Y1)
	c.xLabels(labels)
	return c
}

// ---------- attendance matrix ----------

type matrix struct {
	W, H         float64
	CellW, CellH float64
	Heads        []mhead
	Rows         []mrow
	Empty        bool
}

type mhead struct {
	X       float64
	Num, Wd string
	Weekend bool
}

type mrow struct {
	Y     float64
	Name  string
	Cells []mcell
}

type mcell struct {
	X      float64
	Bucket int
	Title  string
}

// buildMatrix lays the matrix out for a page about 1400 px wide, so it is
// drawn at roughly its natural size: the columns share the width left after
// the names, and the cell height stays fixed.
func (s *Server) buildMatrix(d *dash.Dashboard) matrix {
	const width, nameW, gap, head, cellH = 1400.0, 230.0, 4.0, 34.0, 20.0
	lang, loc := s.Cfg.Lang, s.Cfg.Timezone
	n := max(len(d.Days), 1)
	col := (width - nameW) / float64(n)
	m := matrix{W: width, CellW: math.Max(col-gap, 2), CellH: cellH, Empty: len(d.Students) == 0}
	m.H = head + float64(len(d.Students))*(cellH+gap)
	for i, dd := range d.Days {
		m.Heads = append(m.Heads, mhead{X: nameW + float64(i)*col + m.CellW/2, Num: strconv.Itoa(dd.Date.Day()),
			Wd: strings.ToLower(s.t(fmt.Sprintf("wd.%d", dd.Date.Weekday()))), Weekend: dd.Weekend})
	}
	for r, st := range d.Students {
		row := mrow{Y: head + float64(r)*(cellH+gap), Name: st.Name}
		for i, dd := range d.Days {
			row.Cells = append(row.Cells, mcell{X: nameW + float64(i)*col, Bucket: st.Cells[i],
				Title: s.t("dash.cell", st.Name, i18n.Day(lang, dd.Date.In(loc), false), i18n.Num(lang, st.DayHours[i].Hours(), 1))})
		}
		m.Rows = append(m.Rows, row)
	}
	return m
}

// ---------- CSV ----------

// handleDashboardCSV exports the students table of the dashboard with the
// same filters, plus one hours column per day.
func (s *Server) handleDashboardCSV(w http.ResponseWriter, r *http.Request) {
	d, _, err := s.dashboard(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf") // BOM so Excel opens UTF-8 (Cyrillic names) correctly
	cw := csv.NewWriter(&buf)
	header := []string{"student_id", "name", "group", "status", "hours", "days", "attendance_pct", "hours_per_day", "sessions", "avg_arrival", "last_visit"}
	for _, dd := range d.Days {
		header = append(header, dd.Date.Format("2006-01-02"))
	}
	cw.Write(header)
	f2 := func(x float64) string { return strconv.FormatFloat(x, 'f', 2, 64) }
	for _, st := range d.Students {
		status := "left"
		if st.Active {
			status = "active"
		}
		arrival := ""
		if st.HasArrival {
			m := int(st.AvgArrival / time.Minute)
			arrival = fmt.Sprintf("%02d:%02d", m/60, m%60)
		}
		rec := []string{strconv.FormatInt(st.ID, 10), st.Name, st.Group, status, f2(st.Total.Hours()), strconv.Itoa(st.Days),
			strconv.FormatFloat(math.Round(st.Attendance*1000)/10, 'f', 1, 64), f2(st.PerDay.Hours()), strconv.Itoa(st.Sessions),
			arrival, st.LastVisit.Format("2006-01-02")}
		for _, h := range st.DayHours {
			rec = append(rec, f2(h.Hours()))
		}
		cw.Write(rec)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.serverError(w, err)
		return
	}
	name := fmt.Sprintf("attendance-%s-%s.csv", d.From.Format("2006-01-02"), d.To.AddDate(0, 0, -1).Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(buf.Bytes())
}
