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
	Email       string
	XP          int64
	Audit       float64
	HasAudit    bool
	Events      string
	Synced      time.Time
	StudentID   int64 // 0: no student has this login
	Student     string
	Group       string
}

func (s *Server) platformUsers(ctx context.Context, where string, args ...any) ([]platformUser, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT p.id, p.login, trim(p.last_name || ' ' || p.first_name), p.email, p.xp, p.audit_ratio, p.events, p.synced_at,
			COALESCE(s.id, 0), COALESCE(s.name, ''), COALESCE(s.grp, '')
		FROM platform_users p LEFT JOIN students s ON lower(s.login) = lower(p.login) AND s.login <> '' WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []platformUser
	for rows.Next() {
		var u platformUser
		var audit sql.NullFloat64
		var synced int64
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.XP, &audit, &u.Events, &synced, &u.StudentID, &u.Student, &u.Group); err != nil {
			return nil, err
		}
		u.Audit, u.HasAudit, u.Synced = audit.Float64, audit.Valid, time.UnixMilli(synced)
		out = append(out, u)
	}
	return out, rows.Err()
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
	"xp":      {func(a, b *platformUser) int { return cmp.Compare(a.XP, b.XP) }, true},
	"audit":   {func(a, b *platformUser) int { return cmp.Compare(a.Audit, b.Audit) }, true},
	"student": {func(a, b *platformUser) int { return compareNames(a.Student, b.Student) }, false},
}

type nPlatformData struct {
	Settings  platform.Settings
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
		s.Platform = &platform.Syncer{DB: s.DB, TokenFile: s.Cfg.PlatformTokenPath(), Log: s.Log}
	}
	return s.Platform
}

func (s *Server) handleNewPlatform(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &nPlatformData{HasToken: s.syncer().HasToken(), SyncEvery: s.Cfg.PlatformSyncEvery, Show: q.Get("show")}
	var err error
	if d.Settings, err = platform.LoadSettings(ctx, s.DB); err != nil {
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
			return "/admin/new/platform"
		}
		return "/admin/new/platform?" + v.Encode()
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
	}{{"login", "plat.col_login", false}, {"name", "plat.col_name", false}, {"xp", "plat.col_xp", true}, {"audit", "plat.col_audit", true}, {"student", "plat.col_student", false}} {
		first := map[bool]string{true: "desc", false: "asc"}[platformSorts[h.key].desc]
		head := tableHead{Key: h.key, Label: s.t(h.label), Num: h.num, URL: link(h.key, first, 1)}
		if d.Sort == h.key {
			head.Sort = map[string]string{"asc": "ascending", "desc": "descending"}[d.Dir]
			head.URL = link(h.key, map[string]string{"asc": "desc", "desc": "asc"}[d.Dir], 1)
		}
		d.Heads = append(d.Heads, head)
	}
	s.render(w, r, http.StatusOK, "nplatform", d)
}

func (s *Server) platformError(err error) string {
	switch {
	case errors.Is(err, platform.ErrBadURL):
		return s.t("plat.bad_url")
	case errors.Is(err, platform.ErrBadEvents):
		return s.t("plat.bad_events")
	case errors.Is(err, platform.ErrBadToken):
		return s.t("plat.bad_token")
	case errors.Is(err, platform.ErrBusy):
		return s.t("plat.busy")
	case errors.Is(err, platform.ErrNoToken):
		return s.t("plat.no_token")
	case errors.Is(err, platform.ErrNotSetUp):
		return s.t("plat.not_set_up")
	}
	return s.t("plat.failed", err.Error())
}

func (s *Server) handlePlatformSettings(w http.ResponseWriter, r *http.Request) {
	err := s.syncer().SaveSettings(r.Context(), s.Now(), r.PostFormValue("url"), r.PostFormValue("events"), r.PostFormValue("token"), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/new/platform", s.platformError(err), true)
		return
	}
	back(w, r, "/admin/new/platform", s.t("plat.saved"), false)
}

func (s *Server) handlePlatformSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	res, err := s.syncer().Sync(ctx, s.Now(), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/new/platform", s.platformError(err), true)
		return
	}
	msg := s.t("plat.synced", res.Users, res.Linked)
	if len(res.Warnings) > 0 {
		msg += " " + strings.Join(res.Warnings, "; ")
	}
	back(w, r, "/admin/new/platform", msg, len(res.Warnings) > 0)
}

func (s *Server) handlePlatformLink(w http.ResponseWriter, r *http.Request) {
	links, err := platform.LinkByName(r.Context(), s.DB, s.Now(), staffFrom(r).Username)
	if err != nil {
		back(w, r, "/admin/new/platform", s.coreError(err), true)
		return
	}
	if len(links) == 0 {
		back(w, r, "/admin/new/platform", s.t("plat.linked_none"), false)
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
	back(w, r, "/admin/new/platform", msg, false)
}
