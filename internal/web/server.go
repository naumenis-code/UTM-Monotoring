// Package web implements the HTTP dashboard: a form to register УТМ
// instances, a status list, and a settings page for the polling schedule
// and Telegram recipients.
package web

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/scp-oss/utm-data/internal/auth"
	"github.com/scp-oss/utm-data/internal/models"
	"github.com/scp-oss/utm-data/internal/scheduler"
	"github.com/scp-oss/utm-data/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	store *store.Store
	sched *scheduler.Scheduler
	auth  *auth.Manager
	pages map[string]*template.Template
	mux   *http.ServeMux
}

// pageFiles maps a logical page name to the content template that fills the
// "content" block of layout.html. Each page is parsed as its own template
// set (layout.html + exactly one content file) so that every file can
// define a block named "content" without colliding with the others.
var pageFiles = map[string]string{
	"index":    "templates/index.html",
	"utm_form": "templates/utm_form.html",
	"settings": "templates/settings.html",
	"login":    "templates/login.html",
}

func New(st *store.Store, sched *scheduler.Scheduler, authMgr *auth.Manager) *Server {
	funcs := template.FuncMap{
		"fmtDate":       fmtDate,
		"fmtDateInput":  fmtDateInput,
		"fmtDateTime":   fmtDateTime,
		"certInfo":      newCertInfo,
		"prettyOrgName": models.PrettyOrgName,
		"daysSince":     daysSince,
	}

	pages := make(map[string]*template.Template, len(pageFiles))
	for name, file := range pageFiles {
		pages[name] = template.Must(
			template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", file),
		)
	}

	s := &Server{store: st, sched: sched, auth: authMgr, pages: pages, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleIndex)

	s.mux.HandleFunc("GET /login", s.handleLoginForm)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	s.mux.HandleFunc("GET /utm/new", s.requireAuth(s.handleUTMNewForm))
	s.mux.HandleFunc("POST /utm/new", s.requireAuth(s.handleUTMCreate))
	s.mux.HandleFunc("POST /utm/bulk-add", s.requireAuth(s.handleUTMBulkAdd))
	s.mux.HandleFunc("GET /utm/{id}/edit", s.requireAuth(s.handleUTMEditForm))
	s.mux.HandleFunc("POST /utm/{id}/edit", s.requireAuth(s.handleUTMUpdate))
	s.mux.HandleFunc("POST /utm/{id}/delete", s.requireAuth(s.handleUTMDelete))
	s.mux.HandleFunc("POST /utm/{id}/poll", s.requireAuth(s.handleUTMPollNow))

	s.mux.HandleFunc("GET /settings", s.requireAuth(s.handleSettings))
	s.mux.HandleFunc("POST /settings", s.requireAuth(s.handleSettingsSave))
	s.mux.HandleFunc("GET /settings/export", s.requireAuth(s.handleUTMExport))
	s.mux.HandleFunc("POST /settings/import", s.requireAuth(s.handleUTMImport))
	s.mux.HandleFunc("POST /settings/telegram/add", s.requireAuth(s.handleTelegramChatAdd))
	s.mux.HandleFunc("POST /settings/telegram/{id}/delete", s.requireAuth(s.handleTelegramChatDelete))
	s.mux.HandleFunc("POST /settings/telegram/test", s.requireAuth(s.handleTelegramTest))
}

// requireAuth redirects to the login page when no valid admin session is
// present. When no admin password is configured at all, auth.Manager treats
// every request as authenticated and this is a no-op.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Authenticated(r) {
			target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// flash is a one-off message rendered at the top of the page after a
// redirect. It is carried via query string rather than a cookie/session
// since this is a small internal tool.
type flash struct {
	Kind string
	Text string
}

// base is embedded in every page's view model so the shared layout can
// show/hide admin links and the "no password configured" warning.
type base struct {
	Flash          *flash
	Authenticated  bool
	AuthConfigured bool
}

func (s *Server) pageBase(r *http.Request) base {
	return base{
		Flash:          flashFromRequest(r),
		Authenticated:  s.auth.Authenticated(r),
		AuthConfigured: s.auth.Configured(),
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func redirectWithFlash(w http.ResponseWriter, r *http.Request, target, kind, text string) {
	q := "?flash_kind=" + urlEscape(kind) + "&flash_text=" + urlEscape(text)
	http.Redirect(w, r, target+q, http.StatusSeeOther)
}

func urlEscape(s string) string {
	return template.URLQueryEscaper(s)
}

func flashFromRequest(r *http.Request) *flash {
	kind := r.URL.Query().Get("flash_kind")
	text := r.URL.Query().Get("flash_text")
	if text == "" {
		return nil
	}
	return &flash{Kind: kind, Text: text}
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("02.01.2006")
}

// fmtDateInput formats a date for an HTML <input type=date> value.
func fmtDateInput(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

func fmtDateTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("02.01.2006 15:04")
}

// daysSince reports whole days elapsed since t (0 for "started today"),
// used to show how long a УТМ has been unreachable.
func daysSince(t *time.Time) int {
	if t == nil {
		return 0
	}
	return int(time.Since(*t).Hours() / 24)
}

// certInfo is the view model behind the "certcell" template block.
type certInfo struct {
	From       *time.Time
	To         *time.Time
	DaysLeft   int
	BadgeClass string
}

func newCertInfo(from, to *time.Time) certInfo {
	ci := certInfo{From: from, To: to}
	if to == nil {
		return ci
	}
	days := int(time.Until(*to).Hours() / 24)
	ci.DaysLeft = days
	switch {
	case days < 0:
		ci.BadgeClass = "danger"
	case days <= 10:
		ci.BadgeClass = "danger"
	case days <= 30:
		ci.BadgeClass = "warn"
	default:
		ci.BadgeClass = "ok"
	}
	return ci
}
