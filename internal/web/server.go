// Package web implements the HTTP dashboard: a form to register УТМ
// instances, a status list, and a settings page for the polling schedule
// and Telegram recipients.
package web

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/scp-oss/utm-data/internal/scheduler"
	"github.com/scp-oss/utm-data/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	store *store.Store
	sched *scheduler.Scheduler
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
}

func New(st *store.Store, sched *scheduler.Scheduler) *Server {
	funcs := template.FuncMap{
		"fmtDate":     fmtDate,
		"fmtDateTime": fmtDateTime,
		"certInfo":    newCertInfo,
	}

	pages := make(map[string]*template.Template, len(pageFiles))
	for name, file := range pageFiles {
		pages[name] = template.Must(
			template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", file),
		)
	}

	s := &Server{store: st, sched: sched, pages: pages, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleIndex)

	s.mux.HandleFunc("GET /utm/new", s.handleUTMNewForm)
	s.mux.HandleFunc("POST /utm/new", s.handleUTMCreate)
	s.mux.HandleFunc("GET /utm/{id}/edit", s.handleUTMEditForm)
	s.mux.HandleFunc("POST /utm/{id}/edit", s.handleUTMUpdate)
	s.mux.HandleFunc("POST /utm/{id}/delete", s.handleUTMDelete)
	s.mux.HandleFunc("POST /utm/{id}/poll", s.handleUTMPollNow)

	s.mux.HandleFunc("GET /settings", s.handleSettings)
	s.mux.HandleFunc("POST /settings", s.handleSettingsSave)
	s.mux.HandleFunc("POST /settings/telegram/add", s.handleTelegramChatAdd)
	s.mux.HandleFunc("POST /settings/telegram/{id}/delete", s.handleTelegramChatDelete)
	s.mux.HandleFunc("POST /settings/telegram/test", s.handleTelegramTest)
}

// flash is a one-off message rendered at the top of the page after a
// redirect. It is carried via query string rather than a cookie/session
// since this is a small internal tool.
type flash struct {
	Kind string
	Text string
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
	q := "?flash_kind=" + template.URLQueryEscaper(kind) + "&flash_text=" + template.URLQueryEscaper(text)
	http.Redirect(w, r, target+q, http.StatusSeeOther)
}

func flashFromRequest(r *http.Request) *flash {
	kind := r.URL.Query().Get("flash_kind")
	text := r.URL.Query().Get("flash_text")
	if text == "" {
		return nil
	}
	return &flash{Kind: kind, Text: text}
}

func pollTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 15*time.Second)
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("02.01.2006")
}

func fmtDateTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("02.01.2006 15:04")
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
