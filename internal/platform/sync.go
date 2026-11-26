package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"attendance/internal/core"
	"attendance/internal/store"
)

// Settings keys. The address is fixed in the configuration (platform_url);
// the events are set by an admin in the UI; the access token is kept in a
// file (Syncer.TokenFile), never in the database, so database backups do
// not carry it.
const (
	keyEvents    = "platform_events"
	keyLastTry   = "platform_last_try"
	keyLastSync  = "platform_last_sync"
	keyLastError = "platform_last_error"
	keyResult    = "platform_last_result"
)

// Settings are what an admin sets in the UI: the events whose participants
// are all read, besides the students' own logins. They are optional.
type Settings struct {
	Events []int64
}

// Has reports whether an event is among them.
func (s Settings) Has(id int64) bool { return slices.Contains(s.Events, id) }

// EventsText is how the events are shown and typed: "12, 15".
func (s Settings) EventsText() string {
	parts := make([]string, len(s.Events))
	for i, e := range s.Events {
		parts[i] = strconv.FormatInt(e, 10)
	}
	return strings.Join(parts, ", ")
}

var (
	ErrBadEvents     = errors.New("events are numbers separated by commas, at most 20")
	ErrBadToken      = errors.New("the access token looks wrong: paste it exactly as the platform shows it")
	ErrBusy          = errors.New("a sync is already running")
	ErrNoToken       = errors.New("no access token has been saved")
	ErrNoPlatform    = errors.New("platform_url is empty in the configuration")
	ErrNothingToRead = errors.New("no student has a platform login and no event is set")
)

const maxEvents = 20

// ParseEvents reads "12, 15" (commas or spaces) into sorted distinct ids.
func ParseEvents(s string) ([]int64, error) {
	seen := map[int64]bool{}
	var out []int64
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n <= 0 {
			return nil, ErrBadEvents
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) > maxEvents {
		return nil, ErrBadEvents
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func LoadSettings(ctx context.Context, q store.Querier) (Settings, error) {
	e, err := store.GetSetting(ctx, q, keyEvents)
	if err != nil {
		return Settings{}, err
	}
	events, _ := ParseEvents(e)
	return Settings{Events: events}, nil
}

// Result is what one sync did, shown on the Platform page.
type Result struct {
	Users           int      `json:"users"`
	WithXP          int      `json:"with_xp"`        // users with XP in their current event
	Linked          int      `json:"linked"`         // students whose login is a platform user
	UnknownLogins   int      `json:"unknown_logins"` // students whose login is not among them
	StudentsNoLogin int      `json:"students_no_login"`
	Warnings        []string `json:"warnings,omitempty"`
}

// Status is the state of the last syncs.
type Status struct {
	LastTry, LastSync time.Time
	LastError         string
	Result            Result
}

func LoadStatus(ctx context.Context, q store.Querier) (Status, error) {
	var st Status
	get := func(k string) string {
		v, _ := store.GetSetting(ctx, q, k)
		return v
	}
	if ms, err := strconv.ParseInt(get(keyLastTry), 10, 64); err == nil {
		st.LastTry = store.FromMs(ms)
	}
	if ms, err := strconv.ParseInt(get(keyLastSync), 10, 64); err == nil {
		st.LastSync = store.FromMs(ms)
	}
	st.LastError = get(keyLastError)
	if v := get(keyResult); v != "" {
		json.Unmarshal([]byte(v), &st.Result)
	}
	return st, nil
}

// Syncer runs syncs one at a time: on a schedule (app) and on demand (web, CLI).
type Syncer struct {
	DB        *store.DB
	Base      string // the platform, from platform_url; "": none
	TokenFile string
	HTTP      *http.Client // nil: a client with a 60 s timeout
	Log       *slog.Logger
	mu        sync.Mutex
}

// HasToken reports whether an access token is saved.
func (s *Syncer) HasToken() bool {
	b, err := os.ReadFile(s.TokenFile)
	return err == nil && strings.TrimSpace(string(b)) != ""
}

// SaveToken stores the access token, readable by the service user only.
func (s *Syncer) SaveToken(token string) error {
	token = strings.TrimSpace(token)
	if len(token) < 8 || len(token) > 512 || strings.ContainsAny(token, " \t\r\n") {
		return ErrBadToken
	}
	if s.TokenFile == "" {
		return errors.New("platform_token_file is not set")
	}
	if err := os.MkdirAll(filepath.Dir(s.TokenFile), 0o700); err != nil {
		return err
	}
	tmp := s.TokenFile + ".new"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.TokenFile)
}

// Ready reports whether a sync can run: there is a platform and a token.
func (s *Syncer) Ready() bool { return s.Base != "" && s.HasToken() }

// SaveSettings stores the events, and the token if one is given.
func (s *Syncer) SaveSettings(ctx context.Context, now time.Time, rawEvents, token, actor string) error {
	events, err := ParseEvents(rawEvents)
	if err != nil {
		return err
	}
	if token != "" {
		if err := s.SaveToken(token); err != nil {
			return err
		}
	}
	return s.saveEvents(ctx, now, Settings{Events: events}, token != "", actor)
}

// SetEvent adds an event to the settings or takes it out.
func (s *Syncer) SetEvent(ctx context.Context, now time.Time, id int64, on bool, actor string) error {
	set, err := LoadSettings(ctx, s.DB)
	if err != nil {
		return err
	}
	if id <= 0 {
		return ErrBadEvents
	}
	events := slices.DeleteFunc(slices.Clone(set.Events), func(e int64) bool { return e == id })
	if on {
		events = append(events, id)
		slices.Sort(events)
	}
	if len(events) > maxEvents {
		return ErrBadEvents
	}
	return s.saveEvents(ctx, now, Settings{Events: events}, false, actor)
}

func (s *Syncer) saveEvents(ctx context.Context, now time.Time, set Settings, newToken bool, actor string) error {
	return s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := store.SetSetting(ctx, tx, keyEvents, set.EventsText()); err != nil {
			return err
		}
		details := fmt.Sprintf("events %q", set.EventsText())
		if newToken {
			details += ", new access token"
		}
		return store.Audit(ctx, tx, now, actor, "platform_settings", "", details)
	})
}

// Due reports whether a scheduled sync should run now.
func (s *Syncer) Due(ctx context.Context, now time.Time, every time.Duration) bool {
	if every <= 0 || !s.Ready() {
		return false
	}
	st, err := LoadStatus(ctx, s.DB)
	return err == nil && now.Sub(st.LastTry) >= every
}

// Sync reads the platform and stores what it found. actor is "" for the
// scheduled sync (not journaled) or the staff member who asked for it.
func (s *Syncer) Sync(ctx context.Context, now time.Time, actor string) (Result, error) {
	if !s.mu.TryLock() {
		return Result{}, ErrBusy
	}
	defer s.mu.Unlock()
	if s.Base == "" {
		return Result{}, ErrNoPlatform
	}
	set, err := LoadSettings(ctx, s.DB)
	if err != nil {
		return Result{}, err
	}
	token, err := os.ReadFile(s.TokenFile)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return Result{}, ErrNoToken
	}
	logins, err := studentLogins(ctx, s.DB)
	if err != nil {
		return Result{}, err
	}
	if len(logins) == 0 && len(set.Events) == 0 {
		s.record(now, Result{}, ErrNothingToRead)
		return Result{}, ErrNothingToRead
	}
	c := &Client{Base: s.Base, Token: strings.TrimSpace(string(token)), HTTP: s.HTTP}
	data, warnings, err := c.Fetch(ctx, Request{Logins: logins, Events: set.Events})
	if err != nil {
		err = errors.New(c.redact(err.Error())) // belt and braces: never store the token
		s.record(now, Result{}, err)
		if s.Log != nil {
			s.Log.Warn("platform sync failed", "err", err)
		}
		return Result{}, err
	}
	res, err := Store(ctx, s.DB, now, data)
	if err != nil {
		return Result{}, err
	}
	res.Warnings = warnings
	s.record(now, res, nil)
	if actor != "" {
		store.Audit(ctx, s.DB, now, actor, "platform_sync", "", fmt.Sprintf("%d users, %d linked", res.Users, res.Linked))
	}
	if s.Log != nil {
		s.Log.Info("platform sync", "users", res.Users, "linked", res.Linked, "warnings", len(res.Warnings))
	}
	return res, nil
}

func (s *Syncer) record(now time.Time, res Result, failure error) {
	ctx := context.Background()
	ms := strconv.FormatInt(store.Ms(now), 10)
	store.SetSetting(ctx, s.DB, keyLastTry, ms)
	if failure != nil {
		store.SetSetting(ctx, s.DB, keyLastError, failure.Error())
		return
	}
	b, _ := json.Marshal(res)
	store.SetSetting(ctx, s.DB, keyLastSync, ms)
	store.SetSetting(ctx, s.DB, keyLastError, "")
	store.SetSetting(ctx, s.DB, keyResult, string(b))
}

// studentLogins are the logins of the students who have not left.
func studentLogins(ctx context.Context, q store.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT login FROM students WHERE login <> '' AND archived = 0 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func nullMs(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: store.Ms(t), Valid: true}
}

// Store replaces what the last sync stored and counts the links.
func Store(ctx context.Context, db *store.DB, now time.Time, d Data) (Result, error) {
	var res Result
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{"DELETE FROM platform_users", "DELETE FROM platform_events", "DELETE FROM platform_user_events"} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		for _, e := range d.Events {
			parent := sql.NullInt64{Int64: e.ParentID, Valid: e.ParentID != 0}
			if _, err := tx.ExecContext(ctx, "INSERT INTO platform_events(id, path, parent_id, start_at, end_at) VALUES(?, ?, ?, ?, ?)",
				e.ID, e.Path, parent, nullMs(e.Start), nullMs(e.End)); err != nil {
				return err
			}
		}
		for _, u := range d.Users {
			var cur EventStat
			for _, e := range u.Events {
				if _, err := tx.ExecContext(ctx, "INSERT INTO platform_user_events(user_id, event_id, level, xp) VALUES(?, ?, ?, ?)",
					u.ID, e.EventID, e.Level, e.XP); err != nil {
					return err
				}
				if e.EventID == u.Current {
					cur = e
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO platform_users(id, login, first_name, last_name, other_name, email, audit_ratio, event_id, level, xp, synced_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, u.ID, u.Login, u.FirstName, u.LastName, u.OtherName, u.Email,
				sql.NullFloat64{Float64: u.AuditRatio, Valid: u.HasAuditRatio}, sql.NullInt64{Int64: u.Current, Valid: u.Current != 0},
				cur.Level, cur.XP, store.Ms(now)); err != nil {
				return err
			}
			res.Users++
			if cur.XP != 0 {
				res.WithXP++
			}
		}
		return tx.QueryRowContext(ctx, `SELECT
				COALESCE(SUM(CASE WHEN s.login <> '' AND p.id IS NOT NULL THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN s.login <> '' AND p.id IS NULL THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN s.login = '' THEN 1 ELSE 0 END), 0)
			FROM students s LEFT JOIN platform_users p ON lower(p.login) = lower(s.login)
			WHERE s.archived = 0`).Scan(&res.Linked, &res.UnknownLogins, &res.StudentsNoLogin)
	})
	return res, err
}

// NameLink is a login given to a student because the names matched.
type NameLink struct {
	StudentID int64
	Student   string
	Login     string
}

// LinkByName gives logins to students who have none, where a platform
// user's first and last name (in Cyrillic, or in Latin letters) are both
// among the words of exactly one such student's name (compared as
// core.NameWords does: Нұрлан is Нурлан), that student matches no other platform user, and the
// login is not already a student's. Each link is journaled.
func LinkByName(ctx context.Context, db *store.DB, now time.Time, actor string) ([]NameLink, error) {
	type person struct {
		id    int64
		name  string
		words map[string]bool
	}
	var students []person
	rows, err := db.QueryContext(ctx, "SELECT id, name FROM students WHERE login = '' AND archived = 0 ORDER BY id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.id, &p.name); err != nil {
			rows.Close()
			return nil, err
		}
		p.words = map[string]bool{}
		for _, w := range core.NameWords(p.name) {
			p.words[w] = true
		}
		students = append(students, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type puser struct {
		login string
		names [][]string // the words of each of the user's names
	}
	var users []puser
	rows, err = db.QueryContext(ctx, `SELECT p.login, p.first_name, p.last_name, p.other_name FROM platform_users p
		WHERE NOT EXISTS (SELECT 1 FROM students s WHERE lower(s.login) = lower(p.login) AND s.login <> '') ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u puser
		var first, last, other string
		if err := rows.Scan(&u.login, &first, &last, &other); err != nil {
			rows.Close()
			return nil, err
		}
		fw, lw := core.NameWords(first), core.NameWords(last)
		if len(fw) > 0 && len(lw) > 0 { // both names are needed
			u.names = append(u.names, append(fw, lw...))
		}
		if ow := core.NameWords(other); len(ow) >= 2 { // "First Last" in Latin letters
			u.names = append(u.names, ow)
		}
		if len(u.names) > 0 {
			users = append(users, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	matches := func(p person, u puser) bool {
	names:
		for _, words := range u.names {
			for _, w := range words {
				if !p.words[w] {
					continue names
				}
			}
			return true
		}
		return false
	}
	byStudent := map[int64][]int{} // student -> users matching
	byUser := map[int][]int64{}    // user -> students matching
	for i, u := range users {
		for _, p := range students {
			if matches(p, u) {
				byStudent[p.id] = append(byStudent[p.id], i)
				byUser[i] = append(byUser[i], p.id)
			}
		}
	}
	var out []NameLink
	for _, p := range students {
		us := byStudent[p.id]
		if len(us) != 1 || len(byUser[us[0]]) != 1 {
			continue
		}
		login := users[us[0]].login
		if err := core.SetLogin(ctx, db, now, p.id, login, actor); err != nil {
			return out, err
		}
		out = append(out, NameLink{StudentID: p.id, Student: p.name, Login: login})
	}
	return out, nil
}
