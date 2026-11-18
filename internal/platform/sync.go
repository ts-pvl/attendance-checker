package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"attendance/internal/core"
	"attendance/internal/store"
)

// Settings keys. The address and events are set by an admin in the UI; the
// access token is kept in a file (Syncer.TokenFile), never in the database,
// so database backups do not carry it.
const (
	keyURL       = "platform_url"
	keyEvents    = "platform_events"
	keyLastTry   = "platform_last_try"
	keyLastSync  = "platform_last_sync"
	keyLastError = "platform_last_error"
	keyResult    = "platform_last_result"
)

type Settings struct {
	URL    string
	Events []int64
}

func (s Settings) Configured() bool { return s.URL != "" && len(s.Events) > 0 }

// EventsText is how the events are shown and typed: "12, 15".
func (s Settings) EventsText() string {
	parts := make([]string, len(s.Events))
	for i, e := range s.Events {
		parts[i] = strconv.FormatInt(e, 10)
	}
	return strings.Join(parts, ", ")
}

var (
	ErrBadURL    = errors.New("the platform address must look like https://01.example.org")
	ErrBadEvents = errors.New("events are numbers separated by commas, at most 20")
	ErrBadToken  = errors.New("the access token looks wrong: paste it exactly as the platform shows it")
	ErrBusy      = errors.New("a sync is already running")
	ErrNoToken   = errors.New("no access token has been saved")
	ErrNotSetUp  = errors.New("the platform address and events are not set")
)

// CleanURL keeps the scheme and host of an https address.
func CleanURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(u.Host, " /\\") {
		return "", ErrBadURL
	}
	return "https://" + strings.ToLower(u.Host), nil
}

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
	if len(out) > 20 {
		return nil, ErrBadEvents
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func LoadSettings(ctx context.Context, q store.Querier) (Settings, error) {
	u, err := store.GetSetting(ctx, q, keyURL)
	if err != nil {
		return Settings{}, err
	}
	e, err := store.GetSetting(ctx, q, keyEvents)
	if err != nil {
		return Settings{}, err
	}
	events, _ := ParseEvents(e)
	return Settings{URL: u, Events: events}, nil
}

// Result is what one sync did, shown on the Platform page.
type Result struct {
	Users           int      `json:"users"`
	WithXP          int      `json:"with_xp"`
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

// SaveSettings stores the address and events, and the token if one is given.
func (s *Syncer) SaveSettings(ctx context.Context, now time.Time, rawURL, rawEvents, token, actor string) error {
	u, err := CleanURL(rawURL)
	if err != nil {
		return err
	}
	events, err := ParseEvents(rawEvents)
	if err != nil {
		return err
	}
	if token != "" {
		if err := s.SaveToken(token); err != nil {
			return err
		}
	}
	st := Settings{URL: u, Events: events}
	return s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := store.SetSetting(ctx, tx, keyURL, st.URL); err != nil {
			return err
		}
		if err := store.SetSetting(ctx, tx, keyEvents, st.EventsText()); err != nil {
			return err
		}
		details := fmt.Sprintf("address %q, events %q", st.URL, st.EventsText())
		if token != "" {
			details += ", new access token"
		}
		return store.Audit(ctx, tx, now, actor, "platform_settings", "", details)
	})
}

// Due reports whether a scheduled sync should run now.
func (s *Syncer) Due(ctx context.Context, now time.Time, every time.Duration) bool {
	if every <= 0 {
		return false
	}
	set, err := LoadSettings(ctx, s.DB)
	if err != nil || !set.Configured() || !s.HasToken() {
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
	set, err := LoadSettings(ctx, s.DB)
	if err != nil {
		return Result{}, err
	}
	if !set.Configured() {
		return Result{}, ErrNotSetUp
	}
	token, err := os.ReadFile(s.TokenFile)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return Result{}, ErrNoToken
	}
	c := &Client{Base: set.URL, Token: strings.TrimSpace(string(token)), HTTP: s.HTTP}
	users, warnings, err := c.Fetch(ctx, set.Events)
	if err != nil {
		err = errors.New(c.redact(err.Error())) // belt and braces: never store the token
		s.record(now, Result{}, err)
		if s.Log != nil {
			s.Log.Warn("platform sync failed", "err", err)
		}
		return Result{}, err
	}
	res, err := Store(ctx, s.DB, now, users)
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

// Store replaces the stored platform users and counts the links.
func Store(ctx context.Context, db *store.DB, now time.Time, users []User) (Result, error) {
	var res Result
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM platform_users"); err != nil {
			return err
		}
		for _, u := range users {
			ev := make([]string, len(u.Events))
			for i, e := range u.Events {
				ev[i] = strconv.FormatInt(e, 10)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO platform_users(id, login, first_name, last_name, email, xp, audit_ratio, events, synced_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, u.ID, u.Login, u.FirstName, u.LastName, u.Email, u.XP,
				sql.NullFloat64{Float64: u.AuditRatio, Valid: u.HasAuditRatio}, strings.Join(ev, ","), store.Ms(now)); err != nil {
				return err
			}
			res.Users++
			if u.XP != 0 {
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

// nameWords: lower case, ё as е, hyphens as spaces.
func nameWords(s string) []string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '-' || r == '\t' || r == '.' })
}

// LinkByName gives logins to students who have none, where a platform
// user's first and last name are both among the words of exactly one such
// student's name, that student matches no other platform user, and the
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
		for _, w := range nameWords(p.name) {
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
		words []string
	}
	var users []puser
	rows, err = db.QueryContext(ctx, `SELECT p.login, p.first_name, p.last_name FROM platform_users p
		WHERE NOT EXISTS (SELECT 1 FROM students s WHERE lower(s.login) = lower(p.login) AND s.login <> '') ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u puser
		var first, last string
		if err := rows.Scan(&u.login, &first, &last); err != nil {
			rows.Close()
			return nil, err
		}
		fw, lw := nameWords(first), nameWords(last)
		if len(fw) == 0 || len(lw) == 0 {
			continue // both names are needed
		}
		u.words = append(fw, lw...)
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	matches := func(p person, u puser) bool {
		for _, w := range u.words {
			if !p.words[w] {
				return false
			}
		}
		return true
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
