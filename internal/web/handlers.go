package web

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// costHistoryLookback bounds how far back the blame-detail sparkline looks
// for a service's cost history — enough to show a trend, cheap enough to
// query on every page view.
const costHistoryLookback = 14 * 24 * time.Hour

// dashboardData backs templates/pages/dashboard.html. Every count here is
// derived from real store queries — none of it is placeholder data.
type dashboardData struct {
	pageBase
	UnblamedCount  int
	AwaitingReview int
	ConfirmedCount int
	Recent         []models.BlameEdge
}

// Dashboard handles GET /. Until both a cost source and a deploy source are
// connected there's nothing real to show — send the operator to /setup
// instead of a dashboard full of empty tables.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	if !h.status.Ready() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}

	anomalies, err := h.store.UnblamedAnomalies(r.Context())
	if err != nil {
		http.Error(w, "fetching anomalies", http.StatusInternalServerError)
		return
	}

	// Fetch a larger window than we display so the stat cards reflect real
	// totals, not just the 5 rows shown in the table below them.
	edges, err := h.store.RecentBlameEdges(r.Context(), 100)
	if err != nil {
		http.Error(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	var awaitingReview, confirmed int
	for _, e := range edges {
		switch e.Status {
		case models.BlameStatusResolved:
			awaitingReview++
		case models.BlameStatusConfirmed:
			confirmed++
		}
	}

	recent := edges
	if len(recent) > 5 {
		recent = recent[:5]
	}

	h.templates.renderPage(w, "dashboard.html", dashboardData{
		pageBase:       h.base("dashboard"),
		UnblamedCount:  len(anomalies),
		AwaitingReview: awaitingReview,
		ConfirmedCount: confirmed,
		Recent:         recent,
	})
}

// blameListData backs templates/pages/blame_list.html.
type blameListData struct {
	pageBase
	Edges []models.BlameEdge
}

// BlameList handles GET /blame.
func (h *Handler) BlameList(w http.ResponseWriter, r *http.Request) {
	edges, err := h.store.RecentBlameEdges(r.Context(), 50)
	if err != nil {
		http.Error(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}
	h.templates.renderPage(w, "blame_list.html", blameListData{pageBase: h.base("blame"), Edges: edges})
}

// blameDetailData backs templates/pages/blame_detail.html.
type blameDetailData struct {
	pageBase
	Edge    *models.BlameEdge
	History []models.CostSnapshot
}

// BlameDetail handles GET /blame/{id}.
func (h *Handler) BlameDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid blame edge ID", http.StatusBadRequest)
		return
	}

	edge, err := h.store.BlameEdgeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "fetching blame edge", http.StatusInternalServerError)
		return
	}

	// The cost-history chart reads the same service's real snapshot history
	// leading up to this anomaly. Missing history isn't fatal — the template
	// falls back to a plain message when there aren't enough points to chart.
	var history []models.CostSnapshot
	if edge.CostSnapshot != nil {
		from := edge.CostSnapshot.PeriodStart.Add(-costHistoryLookback)
		to := edge.CostSnapshot.PeriodEnd
		history, err = h.store.CostSnapshotsByService(r.Context(), edge.CostSnapshot.Service, from, to)
		if err != nil {
			slog.Warn("web: fetching cost history for detail chart", "service", edge.CostSnapshot.Service, "err", err)
		}
	}

	h.templates.renderPage(w, "blame_detail.html", blameDetailData{
		pageBase: h.base("blame"),
		Edge:     edge,
		History:  history,
	})
}

// Setup handles GET /setup — the onboarding page shown until a cost source
// and a deploy source are both connected, and reachable afterward from the
// sidebar as a status check. It renders nothing but real config-derived
// status; there's no form here that would need to actually persist anything.
func (h *Handler) Setup(w http.ResponseWriter, r *http.Request) {
	h.templates.renderPage(w, "setup.html", pageBase{Active: "setup", Status: h.status})
}

// anomaliesData backs templates/pages/anomalies.html.
type anomaliesData struct {
	pageBase
	Anomalies []models.CostSnapshot
}

// AnomalyList handles GET /anomalies.
func (h *Handler) AnomalyList(w http.ResponseWriter, r *http.Request) {
	anomalies, err := h.store.UnblamedAnomalies(r.Context())
	if err != nil {
		http.Error(w, "fetching anomalies", http.StatusInternalServerError)
		return
	}
	h.templates.renderPage(w, "anomalies.html", anomaliesData{pageBase: h.base("anomalies"), Anomalies: anomalies})
}

// ConfirmBlame handles POST /blame/{id}/confirm.
func (h *Handler) ConfirmBlame(w http.ResponseWriter, r *http.Request) {
	h.updateStatus(w, r, models.BlameStatusConfirmed)
}

// DismissBlame handles POST /blame/{id}/dismiss.
func (h *Handler) DismissBlame(w http.ResponseWriter, r *http.Request) {
	h.updateStatus(w, r, models.BlameStatusDismissed)
}

// updateStatus applies status and re-renders the fragment the caller is
// swapping: "row" from the blame list page, "badge" from the detail page
// (selected via the hx-vals "view" field each button sends).
func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request, status models.BlameStatus) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid blame edge ID", http.StatusBadRequest)
		return
	}

	if err := h.store.UpdateBlameStatus(r.Context(), id, status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "updating blame edge", http.StatusInternalServerError)
		return
	}

	edge, err := h.store.BlameEdgeByID(r.Context(), id)
	if err != nil {
		http.Error(w, "reloading blame edge", http.StatusInternalServerError)
		return
	}

	if r.FormValue("view") == "badge" {
		h.templates.renderPartial(w, "status_badge.html", edge)
		return
	}
	h.templates.renderPartial(w, "blame_row.html", edge)
}
