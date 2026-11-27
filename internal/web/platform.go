package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"attendance/internal/platform"
)

// The Platform tab of the new admin UI: the learning platform's settings,
// the state of the sync and the platform users with the student each one
// is linked to (by login). Everyone on staff can look; admins change things.

type platformUser struct {
	ID          int64
	Login, Name string
	OtherName   string // the name in Latin letters, if different
	Email       string
	Event       platformEvent // the current event; ID 0: none
	Level, XP   int64         // in the current event
	Audit       float64
	HasAudit    bool
	Synced      time.Time
	StudentID   int64 // 0: no student has this login
	Student     string
	Group       string
}

// platformEvent is an event as the last sync stored it.
type platformEvent struct {
	ID         int64
	Path       string
	Start, End time.Time
}

// Name is the event's path without the campus: "/campus/piscinego" is
// "piscinego".
func (e platformEvent) Name() string {
	p := strings.Trim(e.Path, "/")
	if _, rest, ok := strings.Cut(p, "/"); ok {
		return rest
	}
	return p
}

func msTime(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64)
}

func (s *Server) platformUsers(ctx context.Context, where string, args ...any) ([]platformUser, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT p.id, p.login, trim(p.last_name || ' ' || p.first_name), p.other_name, p.email,
			COALESCE(p.event_id, 0), COALESCE(e.path, ''), e.start_at, e.end_at, p.level, p.xp, p.audit_ratio, p.synced_at,
			COALESCE(s.id, 0), COALESCE(s.name, ''), COALESCE(s.grp, '')
		FROM platform_users p LEFT JOIN platform_events e ON e.id = p.event_id
		LEFT JOIN students s ON lower(s.login) = lower(p.login) AND s.login <> '' WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []platformUser
	for rows.Next() {
		var u platformUser
		var audit sql.NullFloat64
		var start, end sql.NullInt64
		var synced int64
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.OtherName, &u.Email, &u.Event.ID, &u.Event.Path, &start, &end,
			&u.Level, &u.XP, &audit, &synced, &u.StudentID, &u.Student, &u.Group); err != nil {
			return nil, err
		}
		u.Event.Start, u.Event.End = msTime(start), msTime(end)
		u.Audit, u.HasAudit, u.Synced = audit.Float64, audit.Valid, time.UnixMilli(synced)
		out = append(out, u)
	}
	return out, rows.Err()
}

// platformUserEvent is a user's level and XP in one event.
type platformUserEvent struct {
	platformEvent
	Level, XP int64
	Current   bool
}

// userEvents are a platform user's events, the latest first.
func (s *Server) userEvents(ctx context.Context, u *platformUser) ([]platformUserEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.id, e.path, e.start_at, e.end_at, ue.level, ue.xp
		FROM platform_user_events ue JOIN platform_events e ON e.id = ue.event_id
		WHERE ue.user_id = ? ORDER BY e.start_at DESC, e.id DESC`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []platformUserEvent
	for rows.Next() {
		var e platformUserEvent
		var start, end sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Path, &start, &end, &e.Level, &e.XP); err != nil {
			return nil, err
		}
		e.Start, e.End, e.Current = msTime(start), msTime(end), e.ID == u.Event.ID
		out = append(out, e)
	}
	return out, rows.Err()
}

// eventRow is a line of the Platform tab's event list.
type eventRow struct {
	platformEvent
	Students int  // our students in it
	Users    int  // platform users read who are in it
	Loading  bool // all its participants are read (in the settings)
}

// studentEvents lists the top-level events of the users read, with our
// students in each, then the events in the settings that are not among
// them (not found on the platform, or not synced yet).
func (s *Server) studentEvents(ctx context.Context, set platform.Settings) ([]eventRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.id, e.path, e.start_at, e.end_at,
			COUNT(DISTINCT s.id), COUNT(DISTINCT ue.user_id)
		FROM platform_events e JOIN platform_user_events ue ON ue.event_id = e.id
		JOIN platform_users p ON p.id = ue.user_id
		LEFT JOIN students s ON lower(s.login) = lower(p.login) AND s.login <> '' AND s.archived = 0
		WHERE e.parent_id IS NULL
		GROUP BY e.id ORDER BY e.start_at DESC, e.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []eventRow
	listed := map[int64]bool{}
	for rows.Next() {
		var e eventRow
		var start, end sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Path, &start, &end, &e.Students, &e.Users); err != nil {
			return nil, err
		}
		e.Start, e.End, e.Loading = msTime(start), msTime(end), set.Has(e.ID)
		listed[e.ID] = true
		if e.Students > 0 || e.Loading {
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range set.Events {
		if !listed[id] {
			out = append(out, eventRow{platformEvent: platformEvent{ID: id}, Loading: true})
		}
	}
	return out, nil
}

// studentPlatform returns the platform user with this login, or nil.
func (s *Server) studentPlatform(ctx context.Context, login string) (*platformUser, error) {
	if login == "" {
		return nil, nil
	}
	us, err := s.platformUsers(ctx, "lower(p.login) = lower(?)", login)
	if err != nil || len(us) == 0 {
		return nil, err
	}
	return &us[0], nil
}

var platformSorts = map[string]struct {
	cmp  func(a, b *platformUser) int
	desc bool
}{
	"login":   {func(a, b *platformUser) int { return cmp.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login)) }, false},
	"name":    {func(a, b *platformUser) int { return compareNames(a.Name, b.Name) }, false},
	"event":   {func(a, b *platformUser) int { return cmp.Compare(a.Event.Start.UnixMilli(), b.Event.Start.UnixMilli()) }, true},
	"level":   {func(a, b *platformUser) int { return cmp.Compare(a.Level, b.Level) }, true},
	"xp":      {func(a, b *platformUser) int { return cmp.Compare(a.XP, b.XP) }, true},
	"audit":   {func(a, b *platformUser) int { return cmp.Compare(a.Audit, b.Audit) }, true},
	"student": {func(a, b *platformUser) int { return compareNames(a.Student, b.Student) }, false},
}

type nPlatformData struct {
	URL       string // the platform, from platform_url; "": none
	Settings  platform.Settings
	Events    []eventRow
	HasToken  bool
	Status    platform.Status
	SyncEvery time.Duration
	Show      string // "" all, "unlinked"
	Sort, Dir string
	Rows      []platformUser
	Heads     []tableHead
	Pager     pager
	PerOpts   []perOption
	Total     int // all platform users
	Unlinked  int
	NoLogin   int // active students without a login
	Self      string
}

func (s *Server) syncer() *platform.Syncer {
	if s.Platform == nil {
		s.Platform = &platform.Syncer{DB: s.DB, Base: s.Cfg.PlatformURL, TokenFile: s.Cfg.PlatformTokenPath(), Log: s.Log}
	}
	return s.Platform
}

func (s *Server) handlePlatform(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &nPlatformData{URL: s.syncer().Base, HasToken: s.syncer().HasToken(), SyncEvery: s.Cfg.PlatformSyncEvery, Show: q.Get("show")}
	var err error
	if d.Settings, err = platform.LoadSettings(ctx, s.DB); err != nil {
		s.serverError(w, err)
		return
	}
	if d.Events, err = s.studentEvents(ctx, d.Settings); err != nil {
		s.serverError(w, err)
		return
	}
	if d.Status, err = platform.LoadStatus(ctx, s.DB); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM students WHERE login = '' AND archived = 0").Scan(&d.NoLogin); err != nil {
		s.serverError(w, err)
		return
	}
	all, err := s.platformUsers(ctx, "1 = 1")
	if err != nil {
		s.serverError(w, err)
		return
	}
	if d.Show != "unlinked" {
		d.Show = ""
	}
	var list []*platformUser
	for i := range all {
		d.Total++
		if all[i].StudentID == 0 {
			d.Unlinked++
		} else if d.Show == "unlinked" {
			continue
		}
		list = append(list, &all[i])
	}
	d.Sort, d.Dir = q.Get("sort"), q.Get("dir")
	key, ok := platformSorts[d.Sort]
	if !ok {
		d.Sort, key = "login", platformSorts["login"]
	}
	if d.Dir != "asc" && d.Dir != "desc" {
		d.Dir = map[bool]string{true: "desc", false: "asc"}[key.desc]
	}
	slices.SortStableFunc(list, func(a, b *platformUser) int {
		c := key.cmp(a, b)
		if d.Dir == "desc" {
			c = -c
		}
		if c == 0 {
			c = cmp.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login))
		}
		return c
	})
	per := parsePer(q.Get("per"), recentDefaultPer)
	link := func(sortKey, dir string, page int) string {
		v := url.Values{}
		if d.Show != "" {
			v.Set("show", d.Show)
		}
		if sortKey != "login" || dir != "asc" {
			v.Set("sort", sortKey)
			v.Set("dir", dir)
		}
		if per != recentDefaultPer {
			v.Set("per", perValue(per))
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		if len(v) == 0 {
			return "/admin/platform"
		}
		return "/admin/platform?" + v.Encode()
	}
	pg, lo, hi := paginate(len(list), parsePage(q.Get("page")), per, func(p int) string { return link(d.Sort, d.Dir, p) })
	d.Pager = pg
	for _, u := range list[lo:hi] {
		d.Rows = append(d.Rows, *u)
	}
	d.Self = link(d.Sort, d.Dir, pg.Page)
	d.PerOpts = perChoices(per, perPageOptions, s.t("dash.per_all"))
	for _, h := range []struct {
		key, label string
		num        bool
	}{{"login", "plat.col_login", false}, {"name", "plat.col_name", false}, {"event", "plat.col_event", false}, {"level", "plat.col_level", true},
		{"xp", "plat.col_xp", true}, {"audit", "plat.col_audit", true}, {"student", "plat.col_student", false}} {
		first := map[bool]string{true: "desc", false: "asc"}[platformSorts[h.key].desc]
		head := tableHead{Key: h.key, Label: s.t(h.label), Num: h.num, URL: link(h.key, first, 1)}
		if d.Sort == h.key {
			head.Sort = map[string]string{"asc": "ascending", "desc": "descending"}[d.Dir]
			head.URL = link(h.key, map[string]string{"asc": "desc", "desc": "asc"}[d.Dir], 1)
		}
		d.Heads = append(d.Heads, head)
	}
	s.render(w, r, http.StatusOK, "platform", d)
}

func (s *Server) platformError(err error) string {
	switch {
	case errors.Is(err, platform.ErrBadEvents):
		return s.t("plat.bad_events")
	case errors.Is(err, platform.ErrBadToken):
		return s.t("plat.bad_token")
	case errors.Is(err, platform.ErrBusy):
		return s.t("plat.busy")
	case errors.Is(err, platform.ErrNoToken):
		return s.t("plat.no_token")
	case errors.Is(err, platform.ErrNoPlatform):
		return s.t("plat.no_platform")
	case errors.Is(err, platform.ErrNothingToRead):
		return s.t("plat.nothing_to_read")
	}
	return s.t("plat.failed", err.Error())
}

func (s *Server) handlePlatformSettings(w http.ResponseWriter, r *http.Request) {
	err := s.syncer().SaveSettings(r.Context(), s.Now(), r.PostFormValue("events"), r.PostFormValue("token"), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/platform", s.platformError(err), true)
		return
	}
	back(w, r, "/admin/platform", s.t("plat.saved"), false)
}

// handlePlatformEvent starts or stops reading all participants of an event.
func (s *Server) handlePlatformEvent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("event"), 10, 64)
	if err != nil {
		id = 0 // SetEvent refuses it
	}
	on := r.PostFormValue("load") == "1"
	if err := s.syncer().SetEvent(r.Context(), s.Now(), id, on, staffFrom(r).Username); err != nil {
		back(w, r, "/admin/platform", s.platformError(err), true)
		return
	}
	back(w, r, "/admin/platform", s.t("plat.event_saved"), false)
}

func (s *Server) handlePlatformSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	res, err := s.syncer().Sync(ctx, s.Now(), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/platform", s.platformError(err), true)
		return
	}
	msg := s.t("plat.synced", res.Users, res.Linked)
	if len(res.Warnings) > 0 {
		msg += " " + strings.Join(res.Warnings, "; ")
	}
	back(w, r, "/admin/platform", msg, len(res.Warnings) > 0)
}

func (s *Server) handlePlatformLink(w http.ResponseWriter, r *http.Request) {
	links, err := platform.LinkByName(r.Context(), s.DB, s.Now(), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/platform", s.coreError(err), true)
		return
	}
	if len(links) == 0 {
		back(w, r, "/admin/platform", s.t("plat.linked_none"), false)
		return
	}
	var names []string
	for _, l := range links[:min(len(links), 5)] {
		names = append(names, fmt.Sprintf("%s (%s)", l.Student, l.Login))
	}
	msg := s.t("plat.linked", len(links)) + " " + strings.Join(names, ", ")
	if len(links) > 5 {
		msg += fmt.Sprintf(" (+%d)", len(links)-5)
	}
	back(w, r, "/admin/platform", msg, false)
}
