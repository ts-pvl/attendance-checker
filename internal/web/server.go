// Package web serves the LAN endpoints: /event for the PC agents, /qr for
// the check-in screen, /admin for staff and /healthz for the alert script.
package web

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"attendance/internal/bot"
	"attendance/internal/config"
	"attendance/internal/core"
	"attendance/internal/hours"
	"attendance/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Notifier receives closed visits (the bot, or nothing when it is disabled).
type Notifier interface {
	Notify(ctx context.Context, closures []core.Closure)
}

type Server struct {
	Cfg     *config.Config
	DB      *store.DB
	Engine  *core.Engine
	Checkin *core.Checkin
	Hosts   *core.Hosts
	Board   *core.QRBoard
	Bot     *bot.Bot // nil when the bot is disabled
	Calc    hours.Calc
	HostKey string
	Log     *slog.Logger
	Now     func() time.Time

	pages  map[string]*pageTemplate
	logins *loginLimiter
}

func (s *Server) notify(ctx context.Context, cl []core.Closure) {
	if s.Bot != nil && len(cl) > 0 {
		s.Bot.Notify(ctx, cl)
	}
}

// Handler builds the HTTP handler. Call once.
func (s *Server) Handler() (http.Handler, error) {
	if s.Now == nil {
		s.Now = time.Now
	}
	s.logins = newLoginLimiter()
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()

	// PC side: only addresses from the hosts file.
	mux.HandleFunc("POST /event", s.handleEvent)
	mux.HandleFunc("GET /qr", s.handleQRPage)
	mux.HandleFunc("GET /qr/state", s.handleQRState)

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// Staff side: LAN admin networks, then login.
	mux.Handle("GET /admin/login", s.adminNet(http.HandlerFunc(s.handleLoginPage)))
	mux.Handle("POST /admin/login", s.adminNet(http.HandlerFunc(s.handleLogin)))
	mux.Handle("POST /admin/logout", s.staff(s.handleLogout))
	mux.Handle("GET /admin/{$}", s.staff(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.home(r), http.StatusSeeOther)
	}))
	mux.Handle("GET /admin/ui/{which}", s.staff(s.handleUISwitch))
	mux.Handle("GET /admin/dashboard", s.staff(s.handleDashboard))
	mux.Handle("GET /admin/dashboard.csv", s.staff(s.handleDashboardCSV))
	mux.Handle("GET /admin/new/students", s.staff(s.handleNewStudents))
	mux.Handle("GET /admin/new/students/slips", s.staff(s.handleNewSlips))
	mux.Handle("GET /admin/new/students/{id}", s.staff(s.handleNewStudent))
	mux.Handle("GET /admin/new/anomalies", s.staff(s.handleNewAnomalies))
	mux.Handle("GET /admin/new/journal", s.admin(s.handleNewJournal))
	mux.Handle("GET /admin/today", s.staff(s.handleToday))
	mux.Handle("GET /admin/today/live", s.staff(s.handleTodayLive))
	mux.Handle("GET /admin/hours", s.staff(s.handleHours))
	mux.Handle("GET /admin/hours.csv", s.staff(s.handleHoursCSV))
	mux.Handle("GET /admin/students", s.staff(s.handleStudents))
	mux.Handle("GET /admin/students/slips", s.staff(s.handleSlips))
	mux.Handle("GET /admin/students/{id}", s.staff(s.handleStudent))
	mux.Handle("GET /admin/anomalies", s.staff(s.handleAnomalies))
	mux.Handle("GET /admin/audit", s.admin(s.handleAudit))

	mux.Handle("POST /admin/students/import", s.admin(s.handleImport))
	mux.Handle("POST /admin/students/add", s.admin(s.handleAddStudent))
	mux.Handle("POST /admin/students/codes", s.admin(s.handleBulkCodes))
	mux.Handle("POST /admin/students/{id}/code", s.admin(s.handleGenCode))
	mux.Handle("POST /admin/students/{id}/unlink", s.admin(s.handleUnlink))
	mux.Handle("POST /admin/students/{id}/archive", s.admin(s.handleArchive))
	mux.Handle("POST /admin/students/{id}/edit", s.admin(s.handleEditStudent))
	mux.Handle("POST /admin/students/{id}/visits", s.admin(s.handleCorrection))

	return securityHeaders(mux), nil
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		// Not "no-referrer": with it, browsers send "Origin: null" on form
		// POSTs and the login's same-origin check refuses every login.
		// same-origin still sends nothing to other sites (t.me links).
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.ServeHTTP(w, r)
	})
}

// remoteIP returns the TCP peer address. Proxy headers are deliberately
// ignored: the server is reached directly on the LAN.
func remoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

func (s *Server) isAdminNet(ip netip.Addr) bool {
	if _, lab := s.Hosts.ByIP(ip); lab && !s.Cfg.AdminFromLabPCs {
		return false
	}
	for _, p := range s.Cfg.AdminNetworks {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) adminNet(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdminNet(remoteIP(r)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Hosts.ByIP(remoteIP(r)); ok {
		http.Redirect(w, r, "/qr", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/today", http.StatusSeeOther)
}
