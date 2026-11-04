package web

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"attendance/internal/core"
	"attendance/internal/i18n"
	"attendance/internal/store"
)

var sidRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// handleEvent receives reports from the PC agent (idle_logout script).
// Authentication: the request must come from an address in the hosts file
// and carry the shared host key. The PC is identified by its address, never
// by anything in the request body.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	host, ok := s.Hosts.ByIP(ip)
	if !ok {
		s.Log.Warn("event from unlisted address", "ip", ip.String())
		http.Error(w, "unknown host", http.StatusForbidden)
		return
	}
	key := r.Header.Get("X-Host-Key")
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.HostKey)) != 1 {
		s.Log.Warn("event with bad host key", "host", host.Name)
		http.Error(w, "bad host key", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	kind, sid := r.PostForm.Get("kind"), r.PostForm.Get("sid")
	if !sidRe.MatchString(sid) {
		http.Error(w, "bad sid", http.StatusBadRequest)
		return
	}
	var idle sql.NullInt64
	if v := strings.TrimSpace(r.PostForm.Get("idle_ms")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "bad idle_ms", http.StatusBadRequest)
			return
		}
		idle = sql.NullInt64{Int64: n, Valid: true}
	}
	cl, err := s.Engine.AgentEvent(r.Context(), s.Now(), host.Name, kind, sid, idle)
	if err != nil {
		if strings.Contains(err.Error(), "unknown agent event") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.serverError(w, err)
		return
	}
	s.notify(r.Context(), cl)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok\n"))
}

func (s *Server) handleQRPage(w http.ResponseWriter, r *http.Request) {
	host, ok := s.Hosts.ByIP(remoteIP(r))
	if !ok {
		s.render(w, r, http.StatusForbidden, "qr_denied", nil)
		return
	}
	s.render(w, r, http.StatusOK, "qr", map[string]any{
		"Host": host.Name,
		"Texts": map[string]string{
			"refresh": i18n.T(s.Cfg.Lang, "qr.refresh_in"),
			"offline": i18n.T(s.Cfg.Lang, "qr.offline"),
		},
	})
}

type qrState struct {
	State     string        `json:"state"` // "qr" or "login"
	Host      string        `json:"host"`
	Link      string        `json:"link,omitempty"`
	RotateMs  int64         `json:"rotate_ms,omitempty"`
	PeriodMs  int64         `json:"period_ms,omitempty"`
	CheckedIn []qrCheckedIn `json:"checked_in"`
}

type qrCheckedIn struct {
	Name  string `json:"name"`
	Since string `json:"since"`
}

// handleQRState is polled by the QR page every few seconds.
func (s *Server) handleQRState(w http.ResponseWriter, r *http.Request) {
	host, ok := s.Hosts.ByIP(remoteIP(r))
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	now := s.Now()
	st := qrState{State: "login", Host: host.Name, CheckedIn: []qrCheckedIn{}}
	var sessID, lastSeen int64
	err := s.DB.QueryRowContext(r.Context(), "SELECT id, last_seen_at FROM sessions WHERE host = ? AND ended_at IS NULL", host.Name).
		Scan(&sessID, &lastSeen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		s.serverError(w, err)
		return
	case now.Sub(store.FromMs(lastSeen)) <= s.Engine.Timeout():
		tok, until := s.Board.Current(host.ID, now)
		st.State = "qr"
		st.Link = core.StartLink(s.Cfg.BotUsername, tok)
		st.RotateMs = until.Sub(now).Milliseconds()
		st.PeriodMs = s.Cfg.QRRotate.Milliseconds()
		rows, err := s.DB.QueryContext(r.Context(), `SELECT st.name, p.confirmed_at FROM presence p JOIN students st ON st.id = p.student_id
			WHERE p.session_id = ? AND p.ended_at IS NULL ORDER BY p.confirmed_at`, sessID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		for rows.Next() {
			var c qrCheckedIn
			var at int64
			if err := rows.Scan(&c.Name, &at); err != nil {
				rows.Close()
				s.serverError(w, err)
				return
			}
			c.Since = store.FromMs(at).In(s.Cfg.Timezone).Format("15:04")
			st.CheckedIn = append(st.CheckedIn, c)
		}
		rows.Close()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(st)
}

// Health is the state reported by /healthz.
type Health struct {
	OK             bool      `json:"ok"`
	Problems       []string  `json:"problems"`
	LastHeartbeat  time.Time `json:"last_heartbeat"`
	LastSessionEnd string    `json:"last_session_end_reason"`
	BotEnabled     bool      `json:"bot_enabled"`
	BotLastPollOK  time.Time `json:"bot_last_poll_ok"`
	BotLastError   string    `json:"bot_last_error,omitempty"`
	OpenSessions   int       `json:"open_sessions"`
}

// CheckHealth decides whether staff should be alerted. Silence from the PCs
// alone is normal (the lab can be empty at night); it is a problem only if
// the last PC session ended by timeout, i.e. the PCs stopped reporting
// instead of logging out.
func (s *Server) CheckHealth(r *http.Request) (Health, error) {
	ctx := r.Context()
	now := s.Now()
	h := Health{OK: true, Problems: []string{}}
	if err := s.DB.PingContext(ctx); err != nil {
		h.OK = false
		h.Problems = append(h.Problems, "database: "+err.Error())
		return h, nil
	}
	var last sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(last_seen_at) FROM sessions").Scan(&last); err != nil {
		return h, err
	}
	if last.Valid {
		h.LastHeartbeat = store.FromMs(last.Int64)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE ended_at IS NULL").Scan(&h.OpenSessions); err != nil {
		return h, err
	}
	var reason sql.NullString
	err := s.DB.QueryRowContext(ctx, "SELECT end_reason FROM sessions WHERE ended_at IS NOT NULL ORDER BY ended_at DESC LIMIT 1").Scan(&reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	h.LastSessionEnd = reason.String
	if last.Valid && now.Sub(h.LastHeartbeat) > s.Cfg.AlertAfter && h.OpenSessions == 0 &&
		(reason.String == core.ReasonTimeout || reason.String == core.ReasonReplaced) {
		h.OK = false
		h.Problems = append(h.Problems, "no heartbeat from any PC for "+now.Sub(h.LastHeartbeat).Round(time.Minute).String()+
			" and the last session timed out (network, power or server problem?)")
	}
	if s.Bot != nil {
		bs := s.Bot.Status()
		h.BotEnabled = true
		h.BotLastPollOK, h.BotLastError = bs.LastPollOK, bs.LastError
		if now.Sub(bs.LastPollOK) > s.Cfg.AlertAfter && now.Sub(serverStart) > s.Cfg.AlertAfter {
			h.OK = false
			h.Problems = append(h.Problems, "the bot has not reached Telegram for "+now.Sub(bs.LastPollOK).Round(time.Minute).String()+": "+bs.LastError)
		}
	}
	return h, nil
}

var serverStart = time.Now()

// handleHealth serves /healthz to the local alert script only.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !remoteIP(r).IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	h, err := s.CheckHealth(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !h.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(h)
}
