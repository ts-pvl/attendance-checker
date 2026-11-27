package web

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"attendance/internal/bot"
	"attendance/internal/core"
	"attendance/internal/dash"
	"attendance/internal/hours"
	"attendance/internal/store"
)

// The Today tab of the new admin UI: every PC as a tile in a grid that looks
// like the room, and below it the visits that ended today. The page reloads
// both parts every 30 seconds. All times come from the server clock.

const todayLayoutKey = "today_layout" // settings key: the grid as JSON (dash.Layout)

// Tile states.
const (
	pcBusy  = "busy"  // a student is checked in
	pcLogin = "login" // someone is logged in, nobody is checked in
	pcFree  = "free"  // no PC session
)

type pcTile struct {
	Index     int    // cell number, row by row
	Host      string // "" for an empty cell
	State     string
	StudentID int64
	Student   string
	For       time.Duration // busy: since the check-in; login: since the login; free: since the last session ended
	Known     bool          // free: the PC has had a session before
	Stale     bool          // busy or login: no heartbeat for more than staleAfter
	LastSeen  time.Time     // busy or login: the last event from the PC
}

// staleAfter: a PC session with no event for this long (two missed
// heartbeats and a margin) is flagged on the Today tab.
const staleAfter = 150 * time.Second

// todayStatus is the health of the system: is everything still talking to
// the server?
type todayStatus struct {
	LastHeartbeat time.Time   // the last event from any PC
	LastScan      time.Time   // the last check-in
	Bot           *bot.Status // nil when the bot is disabled
	OpenVisits    int
	Stale         int // PCs with a session but no recent heartbeat
}

type recentRow struct {
	Host    string
	Student core.Student
	Len     time.Duration
	End     time.Time
	Reason  string // why the visit ended (core.Reason…)
}

// recentSorts: the sortable columns of the table, ascending, and whether the
// first click sorts the biggest first. "ago" is the time since the session
// ended, so ascending puts the most recent session first.
var recentSorts = map[string]struct {
	cmp  func(a, b *recentRow) int
	desc bool
}{
	"pc": {func(a, b *recentRow) int {
		switch {
		case core.NaturalLess(a.Host, b.Host):
			return -1
		case core.NaturalLess(b.Host, a.Host):
			return 1
		}
		return 0
	}, false},
	"len": {func(a, b *recentRow) int { return cmp.Compare(a.Len, b.Len) }, true},
	"ago": {func(a, b *recentRow) int { return b.End.Compare(a.End) }, false},
}

const (
	recentDefaultSort = "ago"
	recentDefaultPer  = 30
)

type recentOpts struct {
	Group     string
	Sort, Dir string
	Page, Per int // Per 0 = all
}

func parseRecentOpts(q url.Values) recentOpts {
	o := recentOpts{Group: q.Get("group"), Sort: q.Get("sort"), Dir: q.Get("dir"),
		Page: parsePage(q.Get("page")), Per: parsePer(q.Get("per"), recentDefaultPer)}
	key, ok := recentSorts[o.Sort]
	if !ok {
		o.Sort, key = recentDefaultSort, recentSorts[recentDefaultSort]
	}
	if o.Dir != "asc" && o.Dir != "desc" {
		o.Dir = "asc"
		if key.desc {
			o.Dir = "desc"
		}
	}
	return o
}

// url is the page with these table settings; defaults are left out.
func (o recentOpts) url(page int) string {
	q := url.Values{}
	if o.Group != "" {
		q.Set("group", o.Group)
	}
	if def := parseRecentOpts(url.Values{"sort": {o.Sort}}); o.Sort != recentDefaultSort || o.Dir != def.Dir {
		q.Set("sort", o.Sort)
		q.Set("dir", o.Dir)
	}
	if o.Per != recentDefaultPer {
		q.Set("per", perValue(o.Per))
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/admin/today"
	}
	return "/admin/today?" + q.Encode()
}

type recentView struct {
	Opts    recentOpts
	Groups  []string
	PerOpts []perOption
	Heads   []tableHead
	Rows    []recentRow
	Pager   pager
	Any     bool   // a session has ended today (before the group filter)
	Self    string // this page with the current table settings: reloaded every 30 s, and where the layout forms return to
}

type nTodayData struct {
	Rows                  [][]pcTile
	LayoutRows, Cols      int // the saved size; more rows are shown when the PCs do not fit
	MaxSide               int
	Custom                bool
	Busy, NoCheckin, Free int
	Table                 recentView
	Batches               []core.PointsBatch // points given to everyone present today
	MaxPoints             int
	Status                todayStatus
}

func (s *Server) handleToday(w http.ResponseWriter, r *http.Request) {
	d, err := s.newToday(r.Context(), parseRecentOpts(r.URL.Query()))
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "today", d)
}

func (s *Server) hostNames() []string {
	var out []string
	for _, h := range s.Hosts.All() {
		out = append(out, h.Name)
	}
	return out
}

// loadTodayLayout reads the saved grid; a missing or unreadable setting is
// the default grid.
func loadTodayLayout(ctx context.Context, q store.Querier) (dash.Layout, error) {
	v, err := store.GetSetting(ctx, q, todayLayoutKey)
	if err != nil {
		return dash.Layout{}, err
	}
	l := dash.DefaultLayout()
	if v != "" && json.Unmarshal([]byte(v), &l) != nil {
		l = dash.DefaultLayout()
	}
	if !dash.ValidGridSize(l.Rows, l.Cols) {
		l.Rows, l.Cols = dash.GridRows, dash.GridCols
	}
	return l, nil
}

func (s *Server) newToday(ctx context.Context, o recentOpts) (*nTodayData, error) {
	now := s.Now()
	layout, err := loadTodayLayout(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	d := &nTodayData{LayoutRows: layout.Rows, Cols: layout.Cols, MaxSide: dash.GridMaxSide, Custom: layout.Custom()}

	// The state of every PC: open PC sessions, the students checked in on
	// them, and when each PC's last session ended.
	tiles := map[string]pcTile{}
	bySession := map[int64]string{}
	rows, err := s.DB.QueryContext(ctx, "SELECT id, host, started_at, last_seen_at FROM sessions WHERE ended_at IS NULL")
	if err != nil {
		return nil, err
	}
	lastSeen := map[string]time.Time{}
	for rows.Next() {
		var id, started, seen int64
		var host string
		if err := rows.Scan(&id, &host, &started, &seen); err != nil {
			rows.Close()
			return nil, err
		}
		bySession[id] = host
		lastSeen[host] = store.FromMs(seen)
		tiles[host] = pcTile{State: pcLogin, For: since(now, store.FromMs(started))}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT p.session_id, st.id, st.name, p.confirmed_at FROM presence p
		JOIN students st ON st.id = p.student_id WHERE p.ended_at IS NULL ORDER BY p.confirmed_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sid, at int64
		t := pcTile{State: pcBusy}
		if err := rows.Scan(&sid, &t.StudentID, &t.Student, &at); err != nil {
			rows.Close()
			return nil, err
		}
		d.Status.OpenVisits++
		if host, ok := bySession[sid]; ok {
			t.For = since(now, store.FromMs(at))
			tiles[host] = t
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lastEnd := map[string]time.Time{}
	rows, err = s.DB.QueryContext(ctx, "SELECT host, MAX(ended_at) FROM sessions WHERE ended_at IS NOT NULL GROUP BY host")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host string
		var at sql.NullInt64
		if err := rows.Scan(&host, &at); err != nil {
			rows.Close()
			return nil, err
		}
		if at.Valid {
			lastEnd[host] = store.FromMs(at.Int64)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nrows, cells := layout.Place(s.hostNames())
	for r := 0; r < nrows; r++ {
		row := make([]pcTile, layout.Cols)
		for c := range row {
			i := r*layout.Cols + c
			t := pcTile{}
			if host := cells[i]; host != "" {
				var live bool
				if t, live = tiles[host]; !live {
					t = pcTile{State: pcFree}
					if end, ok := lastEnd[host]; ok {
						t.Known, t.For = true, since(now, end)
					}
				}
				t.Host = host
				if live {
					t.LastSeen = lastSeen[host]
					t.Stale = now.Sub(t.LastSeen) > staleAfter
					if t.Stale {
						d.Status.Stale++
					}
				}
				switch t.State {
				case pcBusy:
					d.Busy++
				case pcLogin:
					d.NoCheckin++
				default:
					d.Free++
				}
			}
			t.Index = i
			row[c] = t
		}
		d.Rows = append(d.Rows, row)
	}

	if d.Table, err = s.recentSessions(ctx, now, o); err != nil {
		return nil, err
	}
	day := s.Calc.PeriodAt("day", now)
	if d.Batches, err = core.Batches(ctx, s.DB, day.From, day.To); err != nil {
		return nil, err
	}
	d.MaxPoints = core.MaxPoints
	if err := s.loadTodayStatus(ctx, &d.Status); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Server) loadTodayStatus(ctx context.Context, st *todayStatus) error {
	var lastHB, lastScan sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(last_seen_at) FROM sessions").Scan(&lastHB); err != nil {
		return err
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(confirmed_at) FROM presence").Scan(&lastScan); err != nil {
		return err
	}
	if lastHB.Valid {
		st.LastHeartbeat = store.FromMs(lastHB.Int64)
	}
	if lastScan.Valid {
		st.LastScan = store.FromMs(lastScan.Int64)
	}
	if s.Bot != nil {
		b := s.Bot.Status()
		st.Bot = &b
	}
	return nil
}

func since(now, t time.Time) time.Duration { return max(now.Sub(t), 0) }

// recentSessions lists the visits that ended today (local day), with
// corrections applied: the same visits as everywhere else (hours.Load).
func (s *Server) recentSessions(ctx context.Context, now time.Time, o recentOpts) (recentView, error) {
	v := recentView{Opts: o}
	day := s.Calc.PeriodAt("day", now)
	visits, err := hours.Load(ctx, s.DB, day.From, day.To, 0)
	if err != nil {
		return v, err
	}
	students, err := s.loadStudents(ctx)
	if err != nil {
		return v, err
	}
	if v.Groups, err = s.groups(ctx); err != nil {
		return v, err
	}
	if o.Group != "" && !slices.Contains(v.Groups, o.Group) {
		v.Groups = append(v.Groups, o.Group) // keep a chosen group selectable
		slices.Sort(v.Groups)
	}
	var list []*recentRow
	for _, vis := range visits {
		if vis.Deleted || vis.Open || vis.End.Before(day.From) || !vis.End.Before(day.To) {
			continue
		}
		v.Any = true
		st := students[vis.StudentID]
		if o.Group != "" && st.Group != o.Group {
			continue
		}
		st.ID = vis.StudentID
		list = append(list, &recentRow{Host: vis.Host, Student: st, Len: vis.Duration(), End: vis.End, Reason: vis.EndReason})
	}
	by := recentSorts[o.Sort].cmp
	slices.SortStableFunc(list, func(a, b *recentRow) int {
		c := by(a, b)
		if o.Dir == "desc" {
			c = -c
		}
		if c == 0 { // ties: latest end, then PC, then name
			if c = b.End.Compare(a.End); c == 0 {
				if c = recentSorts["pc"].cmp(a, b); c == 0 {
					c = compareNames(a.Student.Name, b.Student.Name)
				}
			}
		}
		return c
	})
	pg, lo, hi := paginate(len(list), o.Page, o.Per, o.url)
	v.Pager, v.Opts.Page = pg, pg.Page
	for _, row := range list[lo:hi] {
		v.Rows = append(v.Rows, *row)
	}
	v.Self = v.Opts.url(v.Opts.Page)
	v.PerOpts = perChoices(o.Per, perPageOptions, s.t("dash.per_all"))
	for _, h := range []struct {
		key, label string
		num        bool
	}{{"pc", "c.pc", false}, {"", "c.student", false}, {"", "c.group", false}, {"len", "ntoday.col_len", true}, {"ago", "ntoday.col_ago", true}, {"", "c.ended", false}} {
		head := tableHead{Key: h.key, Label: s.t(h.label), Num: h.num}
		if h.key != "" {
			next := o
			next.Sort = h.key
			next.Dir = parseRecentOpts(url.Values{"sort": {h.key}}).Dir // first click: the column's natural order
			if o.Sort == h.key {
				head.Sort = map[string]string{"asc": "ascending", "desc": "descending"}[o.Dir]
				next.Dir = map[string]string{"asc": "desc", "desc": "asc"}[o.Dir] // second click flips it
			}
			head.URL = next.url(1)
		}
		v.Heads = append(v.Heads, head)
	}
	return v, nil
}

// handleTodayLayout changes the PC grid (admins): action "swap" exchanges
// the cell of PC "host" with cell "to", "resize" sets "rows" and "cols",
// "reset" goes back to the default grid. Every change is in the journal.
func (s *Server) handleTodayLayout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hosts := s.hostNames()
	var problem string
	err := s.DB.InTx(ctx, func(tx *sql.Tx) error {
		l, err := loadTodayLayout(ctx, tx)
		if err != nil {
			return err
		}
		var details string
		switch r.PostFormValue("action") {
		case "swap":
			_, cells := l.Place(hosts)
			host := r.PostFormValue("host")
			from := slices.Index(cells, host)
			to, err := strconv.Atoi(r.PostFormValue("to"))
			if host == "" || from < 0 || err != nil {
				problem = s.t("ntoday.bad_move")
				return nil
			}
			if from == to {
				return nil // dropped where it was
			}
			var ok bool
			if l, ok = l.Swap(hosts, from, to); !ok {
				problem = s.t("ntoday.bad_move")
				return nil
			}
			if other := cells[to]; other != "" {
				details = fmt.Sprintf("swap %s and %s", host, other)
			} else {
				details = fmt.Sprintf("move %s to cell %d", host, to+1)
			}
		case "resize":
			rows, err1 := strconv.Atoi(r.PostFormValue("rows"))
			cols, err2 := strconv.Atoi(r.PostFormValue("cols"))
			var ok bool
			if l, ok = l.Resize(hosts, rows, cols); err1 != nil || err2 != nil || !ok {
				problem = s.t("ntoday.bad_size", dash.GridMaxSide)
				return nil
			}
			details = fmt.Sprintf("resize to %d x %d", rows, cols)
		case "reset":
			l, details = dash.DefaultLayout(), "reset"
		default:
			problem = s.t("ntoday.bad_move")
			return nil
		}
		b, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if err := store.SetSetting(ctx, tx, todayLayoutKey, string(b)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, s.Now(), staffFrom(r).Username, "today_layout", "", details)
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	back(w, r, "/admin/today", problem, problem != "")
}
