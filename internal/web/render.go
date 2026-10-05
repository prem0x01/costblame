package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
)

//go:embed templates static
var assets embed.FS

// funcMap provides the small set of formatting helpers templates need.
// Arithmetic/formatting is done here rather than in templates so page and
// partial templates share identical output.
var funcMap = template.FuncMap{
	"pct":       func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"signedPct": func(f float64) string { return fmt.Sprintf("%+.1f%%", f) },
	"money":     func(f float64) string { return fmt.Sprintf("$%.2f", f) },
	"sparkline": sparkline,
}

// templateSet holds one combined (layout+partials+page) template per page,
// plus a standalone partials set for htmx fragment responses.
type templateSet struct {
	pages    map[string]*template.Template
	partials *template.Template
}

func loadTemplates() (*templateSet, error) {
	pageFiles, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}

	ts := &templateSet{pages: make(map[string]*template.Template, len(pageFiles))}
	for _, pf := range pageFiles {
		t, err := template.New(filepath.Base(pf)).Funcs(funcMap).ParseFS(
			assets, "templates/layout.html", "templates/partials/*.html", pf,
		)
		if err != nil {
			return nil, fmt.Errorf("parsing page %s: %w", pf, err)
		}
		ts.pages[filepath.Base(pf)] = t
	}

	partials, err := template.New("partials").Funcs(funcMap).ParseFS(assets, "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing partials: %w", err)
	}
	ts.partials = partials

	return ts, nil
}

// renderPage renders pageName (e.g. "dashboard.html") through layout.html.
func (ts *templateSet) renderPage(w http.ResponseWriter, pageName string, data any) {
	t, ok := ts.pages[pageName]
	if !ok {
		http.Error(w, "page not found: "+pageName, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		slog.Error("web: render page", "page", pageName, "err", err)
	}
}

// renderPartial renders a single named partial (e.g. "blame_row.html") with
// no layout — used for htmx fragment swaps.
func (ts *templateSet) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ts.partials.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("web: render partial", "partial", name, "err", err)
	}
}

// staticFileSystem returns the embedded "static" subtree rooted at itself,
// so it can be served directly at /static/ without leaking the "static/" prefix.
func staticFileSystem() (fs.FS, error) {
	return fs.Sub(assets, "static")
}
