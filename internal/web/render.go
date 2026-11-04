package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"attendance/internal/i18n"
)

type pageTemplate = template.Template

// Pages that use the admin layout; qr pages are standalone.
var layoutPages = []string{"login", "today", "hours", "students", "student", "slips", "anomalies", "audit"}

type pageData struct {
	Lang     string
	Page     string
	Staff    *staffCtx
	Flash    string
	FlashErr bool
	D        any
}

func (s *Server) funcs() template.FuncMap {
	lang, loc := s.Cfg.Lang, s.Cfg.Timezone
	format := func(layout string) func(time.Time) string {
		return func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.In(loc).Format(layout)
		}
	}
	return template.FuncMap{
		"t":       func(key string, args ...any) string { return i18n.T(lang, key, args...) },
		"dur":     func(d time.Duration) string { return i18n.Duration(lang, d) },
		"reason":  func(r string) string { return i18n.Reason(lang, r) },
		"clock":   format("15:04"),
		"dt":      format("2006-01-02 15:04"),
		"date":    format("2006-01-02"),
		"inputdt": format("2006-01-02T15:04"),
		"weekday": func(t time.Time) string {
			return i18n.T(lang, fmt.Sprintf("wd.%d", t.In(loc).Weekday())) + " " + t.In(loc).Format("02.01")
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return i18n.T(lang, "c.never")
			}
			d := s.Now().Sub(t)
			if d < time.Minute {
				return i18n.T(lang, "c.just_now")
			}
			return i18n.T(lang, "c.ago", i18n.Duration(lang, d))
		},
		"ms":  func(ms int64) string { return fmt.Sprintf("%.0f s", float64(ms)/1000) },
		"add": func(a, b int) int { return a + b },
		// Audit targets look like "student:12".
		"hasStudent": func(target string) bool { return strings.HasPrefix(target, "student:") },
		"studentID":  func(target string) string { return strings.TrimPrefix(target, "student:") },
	}
}

func (s *Server) parseTemplates() error {
	s.pages = map[string]*pageTemplate{}
	for _, name := range layoutPages {
		t, err := template.New("layout.html").Funcs(s.funcs()).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	for _, name := range []string{"qr", "qr_denied"} {
		t, err := template.New(name+".html").Funcs(s.funcs()).ParseFS(templateFS, "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// render executes a page into a buffer first, so a template error never
// produces half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, d any) {
	s.renderBlock(w, r, status, name, "", d)
}

// renderBlock renders one named block of a page (htmx partials) or, with
// block "", the whole page.
func (s *Server) renderBlock(w http.ResponseWriter, r *http.Request, status int, name, block string, d any) {
	t, ok := s.pages[name]
	if !ok {
		s.serverError(w, fmt.Errorf("no template %q", name))
		return
	}
	pd := pageData{Lang: s.Cfg.Lang, Page: name, Staff: staffFrom(r), D: d}
	if block == "" {
		pd.Flash, pd.FlashErr = readFlash(w, r)
	}
	var buf bytes.Buffer
	var err error
	if block == "" {
		err = t.Execute(&buf, pd)
	} else {
		err = t.ExecuteTemplate(&buf, block, pd)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

const flashCookie = "att_flash"

type flash struct {
	Msg string `json:"m"`
	Err bool   `json:"e"`
}

func setFlash(w http.ResponseWriter, msg string, isErr bool) {
	b, _ := json.Marshal(flash{msg, isErr})
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString(b), Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 60,
	})
}

func readFlash(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", false
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/admin", MaxAge: -1})
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", false
	}
	var f flash
	if json.Unmarshal(b, &f) != nil {
		return "", false
	}
	return f.Msg, f.Err
}

// back redirects to the referring admin page (or fallback) with a flash message.
func back(w http.ResponseWriter, r *http.Request, fallback, msg string, isErr bool) {
	if msg != "" {
		setFlash(w, msg, isErr)
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}
