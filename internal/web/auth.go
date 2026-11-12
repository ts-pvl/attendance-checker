package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"attendance/internal/core"
	"attendance/internal/store"
	"attendance/internal/token"
)

const sessionCookie = "att_session"

type ctxKey int

const staffKey ctxKey = 0

type staffCtx struct {
	core.Staff
	CSRF string
}

func staffFrom(r *http.Request) *staffCtx {
	s, _ := r.Context().Value(staffKey).(*staffCtx)
	return s
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// staff wraps a handler that needs any logged-in staff member.
func (s *Server) staff(h http.HandlerFunc) http.Handler {
	return s.adminNet(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, err := s.loadSession(r)
		if err != nil {
			s.Log.Error("load staff session", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if st == nil {
			if r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			} else {
				w.Header().Set("HX-Redirect", "/admin/login")
				http.Error(w, "login required", http.StatusUnauthorized)
			}
			return
		}
		if r.Method == http.MethodPost && !validCSRF(r, st.CSRF) {
			http.Error(w, "invalid or missing CSRF token; reload the page", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), staffKey, st)))
	}))
}

// admin wraps a handler that changes data: admins only.
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return s.staff(func(w http.ResponseWriter, r *http.Request) {
		if !staffFrom(r).IsAdmin() {
			http.Error(w, "forbidden: read-only account", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

func validCSRF(r *http.Request, want string) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.PostFormValue("csrf")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) loadSession(r *http.Request) (*staffCtx, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	now := s.Now()
	h := hashToken(c.Value)
	var st staffCtx
	var created, seen int64
	err = s.DB.QueryRowContext(r.Context(), `SELECT st.id, st.username, st.role, st.disabled, ss.csrf, ss.created_at, ss.last_seen_at
		FROM staff_sessions ss JOIN staff st ON st.id = ss.staff_id WHERE ss.token_hash = ?`, h).
		Scan(&st.ID, &st.Username, &st.Role, &st.Disabled, &st.CSRF, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Disabled || now.Sub(store.FromMs(seen)) > s.Cfg.StaffSessionIdle || now.Sub(store.FromMs(created)) > s.Cfg.StaffSessionMax {
		_, err := s.DB.ExecContext(r.Context(), "DELETE FROM staff_sessions WHERE token_hash = ?", h)
		return nil, err
	}
	if now.Sub(store.FromMs(seen)) > time.Minute {
		if _, err := s.DB.ExecContext(r.Context(), "UPDATE staff_sessions SET last_seen_at = ? WHERE token_hash = ?", store.Ms(now), h); err != nil {
			return nil, err
		}
	}
	return &st, nil
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if st, _ := s.loadSession(r); st != nil {
		http.Redirect(w, r, s.home(r), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", nil)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Login has no session yet, so no CSRF token; reject cross-site posts by origin.
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	user := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	pass := r.PostFormValue("password")
	ip := remoteIP(r).String()
	now := s.Now()
	if !s.logins.allowed(now, ip, user) {
		s.Log.Warn("staff login throttled", "ip", ip, "user", user)
		s.render(w, r, http.StatusTooManyRequests, "login", map[string]any{"Error": "login.locked", "Username": user})
		return
	}
	st, err := core.Authenticate(r.Context(), s.DB, user, pass)
	if errors.Is(err, core.ErrBadLogin) {
		s.logins.fail(now, ip, user)
		s.Log.Warn("staff login failed", "ip", ip, "user", user)
		s.render(w, r, http.StatusUnauthorized, "login", map[string]any{"Error": "login.failed", "Username": user})
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.logins.success(ip, user)
	tok := base64.RawURLEncoding.EncodeToString(token.NewSecret(32))
	csrf := base64.RawURLEncoding.EncodeToString(token.NewSecret(24))
	if _, err := s.DB.ExecContext(r.Context(),
		"INSERT INTO staff_sessions(token_hash, staff_id, csrf, created_at, last_seen_at) VALUES(?, ?, ?, ?, ?)",
		hashToken(tok), st.ID, csrf, store.Ms(now), store.Ms(now)); err != nil {
		s.serverError(w, err)
		return
	}
	s.Log.Info("staff login", "user", st.Username, "ip", ip)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		MaxAge: int(s.Cfg.StaffSessionMax.Seconds()),
	})
	http.Redirect(w, r, s.home(r), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.DB.ExecContext(r.Context(), "DELETE FROM staff_sessions WHERE token_hash = ?", hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// sameOrigin accepts requests whose Origin (or Referer) matches the Host.
// Browsers always send Origin on form POSTs; a missing header is accepted
// for non-browser clients.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		o = r.Header.Get("Referer")
	}
	if o == "" {
		return true
	}
	o = strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://")
	host, _, _ := strings.Cut(o, "/")
	return host == r.Host
}

// PruneStaffSessions deletes expired staff sessions.
func PruneStaffSessions(ctx context.Context, db *store.DB, now time.Time, idle, max time.Duration) error {
	_, err := db.ExecContext(ctx, "DELETE FROM staff_sessions WHERE last_seen_at < ? OR created_at < ?",
		store.Ms(now.Add(-idle)), store.Ms(now.Add(-max)))
	return err
}

// loginLimiter locks an IP or a username out after repeated failures:
// 5 failures within 15 minutes block further attempts for 15 minutes.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

const (
	loginMaxFails = 5
	loginWindow   = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string][]time.Time{}} }

func (l *loginLimiter) recent(now time.Time, key string) int {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > loginWindow {
		i++
	}
	l.fails[key] = ts[i:]
	if len(l.fails[key]) == 0 {
		delete(l.fails, key)
	}
	return len(ts) - i
}

func (l *loginLimiter) allowed(now time.Time, ip, user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recent(now, "ip:"+ip) < loginMaxFails && l.recent(now, "user:"+user) < loginMaxFails
}

func (l *loginLimiter) failed(now time.Time, key string) {
	l.fails[key] = append(l.fails[key], now)
}

func (l *loginLimiter) fail(now time.Time, ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed(now, "ip:"+ip)
	l.failed(now, "user:"+user)
}

func (l *loginLimiter) success(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, "ip:"+ip)
	delete(l.fails, "user:"+user)
}
