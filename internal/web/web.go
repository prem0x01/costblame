// Package web renders the costblame browser UI: server-rendered HTML pages
// progressively enhanced with htmx. It reads through the same BlameStore
// interface as the JSON API (internal/api/handlers) and mounts on the clean
// paths ("/", "/blame", "/anomalies"), leaving the JSON API under "/api".
package web

import (
	"net/http"

	"github.com/prem0x01/costblame/internal/api/handlers"
)

// Handler serves the costblame HTML UI.
type Handler struct {
	store     handlers.BlameStore
	status    SystemStatus
	templates *templateSet
}

// New creates a web Handler. status reports which real integrations this
// instance has configured (see SystemStatus) so pages can guide a new
// operator instead of just showing empty tables. It panics if the embedded
// templates fail to parse — that's a build-time defect, not a runtime
// condition to recover from.
func New(store handlers.BlameStore, status SystemStatus) *Handler {
	ts, err := loadTemplates()
	if err != nil {
		panic("web: loading templates: " + err.Error())
	}
	return &Handler{store: store, status: status, templates: ts}
}

// pageBase carries the fields every page template needs regardless of its
// own data — which nav item is active and the real system status — via
// struct embedding, so content templates are untouched and layout.html can
// read .Active / .Status directly off any page's data.
type pageBase struct {
	Active string
	Status SystemStatus
}

func (h *Handler) base(active string) pageBase {
	return pageBase{Active: active, Status: h.status}
}

// Register mounts every UI route and the static asset handler on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.Dashboard)
	mux.HandleFunc("GET /setup", h.Setup)
	mux.HandleFunc("GET /blame", h.BlameList)
	mux.HandleFunc("GET /blame/{id}", h.BlameDetail)
	mux.HandleFunc("POST /blame/{id}/confirm", h.ConfirmBlame)
	mux.HandleFunc("POST /blame/{id}/dismiss", h.DismissBlame)
	mux.HandleFunc("GET /anomalies", h.AnomalyList)

	staticFS, err := staticFileSystem()
	if err != nil {
		panic("web: mounting static assets: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
}
