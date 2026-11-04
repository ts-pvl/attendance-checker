package web

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// handleHoursCSV exports exactly what the Hours screen shows (same query,
// same rules), plus one minutes column per day.
func (s *Server) handleHoursCSV(w http.ResponseWriter, r *http.Request) {
	d, err := s.hoursData(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	loc := s.Cfg.Timezone
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(loc).Format("2006-01-02 15:04")
	}
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf") // BOM so Excel opens UTF-8 (Cyrillic names) correctly
	cw := csv.NewWriter(&buf)
	header := []string{"student_id", "name", "group", "period", "from", "to", "total_minutes", "total_hours", "visits", "first_arrival", "last_departure"}
	for _, day := range d.DayCols {
		header = append(header, day.Format("2006-01-02"))
	}
	cw.Write(header)
	for _, row := range d.Rows {
		rec := []string{
			strconv.FormatInt(row.Student.ID, 10), row.Student.Name, row.Student.Group,
			d.Period.Kind, d.Period.From.Format("2006-01-02"), d.Period.To.AddDate(0, 0, -1).Format("2006-01-02"),
			strconv.FormatInt(int64(row.Row.Total/time.Minute), 10),
			strconv.FormatFloat(row.Row.Total.Hours(), 'f', 2, 64),
			strconv.Itoa(row.Row.Visits), stamp(row.Row.FirstArrival), stamp(row.Row.LastDeparture),
		}
		for _, m := range row.Days {
			rec = append(rec, strconv.FormatInt(int64(m/time.Minute), 10))
		}
		cw.Write(rec)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.serverError(w, err)
		return
	}
	name := fmt.Sprintf("hours-%s-%s.csv", d.Period.Kind, d.Period.From.Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(buf.Bytes())
}
