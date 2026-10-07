package web

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// costHistoryLookback bounds how far back the blame-detail sparkline looks
// for a service's cost history — enough to show a trend, cheap enough to
// query on every page view.
const costHistoryLookback = 14 * 24 * time.Hour

// Page sizes. The lists are capped, not paged, for now: the page says how many
// rows match so a truncated list is never mistaken for the whole story.
const (
	blameListLimit   = 50
	anomalyListLimit = 50
	dashboardRecent  = 5
)

// tab is one choice in a filter bar. Count is the number of rows behind it.
type tab struct {
	Label  string
	Href   string
	Count  int
	Active bool
}

// dashboardData backs templates/pages/dashboard.html. Every figure is an exact
// COUNT from the store, not derived from a truncated list.
type dashboardData struct {
	pageBase
	Unblamed   int // anomalies with no candidate deploy
	Candidates int // anomalies with candidates nobody has acted on: need a look
	Resolved   int // edges that triggered an alert and await review
	Confirmed  int
	Recent     []models.BlameEdge
}

// Dashboard handles GET /. Until both a cost source and a deploy source are
// connected there's nothing real to show — send the operator to /setup
// instead of a dashboard full of empty tables.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	if !h.status.Ready() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}

	anomalyCounts, err := h.store.AnomalyStateCounts(r.Context())
	if err != nil {
		http.Error(w, "counting anomalies", http.StatusInternalServerError)
		return
	}
	edgeCounts, err := h.store.BlameStatusCounts(r.Context())
	if err != nil {
		http.Error(w, "counting blame edges", http.StatusInternalServerError)
		return
	}
	recent, err := h.store.ListBlameEdges(r.Context(), models.BlameEdgeFilter{Statuses: models.ActiveBlameStatuses, Limit: dashboardRecent})
	if err != nil {
		http.Error(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	h.templates.renderPage(w, "dashboard.html", dashboardData{
		pageBase:   h.base("dashboard"),
		Unblamed:   anomalyCounts[models.AnomalyStateUnblamed],
		Candidates: anomalyCounts[models.AnomalyStateCandidates],
		Resolved:   edgeCounts[models.BlameStatusResolved],
		Confirmed:  edgeCounts[models.BlameStatusConfirmed],
		Recent:     recent,
	})
}

// blameListData backs templates/pages/blame_list.html.
type blameListData struct {
	pageBase
	Edges    []models.BlameEdge
	Tabs     []tab
	Selected string // the active tab's value
	Matching int    // edges matching the filter, of which Edges shows the newest
}

// blameTabs lists the status filters. "active" (the default) is everything
// except dismissed, and deliberately includes pending: those candidates are what
// most anomalies end up with and they are the ones needing a human.
var blameTabs = []struct{ Value, Label string }{
	{"active", "Active"}, {"pending", "Pending"}, {"resolved", "Resolved"},
	{"confirmed", "Confirmed"}, {"dismissed", "Dismissed"}, {"all", "All"},
}

func countStatuses(counts map[models.BlameStatus]int, statuses []models.BlameStatus) int {
	n := 0
	for _, st := range statuses {
		n += counts[st]
	}
	return n
}

// BlameList handles GET /blame. ?status= picks a filter (default "active").
func (h *Handler) BlameList(w http.ResponseWriter, r *http.Request) {
	selected := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if selected == "" {
		selected = "active"
	}
	statuses, err := models.ParseBlameStatuses(selected)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	counts, err := h.store.BlameStatusCounts(r.Context())
	if err != nil {
		http.Error(w, "counting blame edges", http.StatusInternalServerError)
		return
	}
	edges, err := h.store.ListBlameEdges(r.Context(), models.BlameEdgeFilter{Statuses: statuses, Limit: blameListLimit})
	if err != nil {
		http.Error(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	tabs := make([]tab, 0, len(blameTabs))
	for _, t := range blameTabs {
		ts, _ := models.ParseBlameStatuses(t.Value)
		tabs = append(tabs, tab{Label: t.Label, Href: "/blame?status=" + t.Value, Count: countStatuses(counts, ts), Active: t.Value == selected})
	}
	h.templates.renderPage(w, "blame_list.html", blameListData{
		pageBase: h.base("blame"), Edges: edges, Tabs: tabs, Selected: selected, Matching: countStatuses(counts, statuses),
	})
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
	Anomalies []models.AnomalySummary
	Tabs      []tab
	Selected  string
	Matching  int
}

// anomalyTabs lists the state filters, in the order a reader triages them.
// "candidates" are anomalies where deploys were found but none was confident
// enough to alert: they used to vanish from every list.
var anomalyTabs = []struct {
	Value, Label string
	State        models.AnomalyState
}{
	{"all", "All", ""},
	{"candidates", "Candidates", models.AnomalyStateCandidates},
	{"resolved", "Resolved", models.AnomalyStateResolved},
	{"unblamed", "Unblamed", models.AnomalyStateUnblamed},
	{"confirmed", "Confirmed", models.AnomalyStateConfirmed},
	{"dismissed", "Dismissed", models.AnomalyStateDismissed},
}

// AnomalyList handles GET /anomalies. ?state= picks a filter (default "all").
func (h *Handler) AnomalyList(w http.ResponseWriter, r *http.Request) {
	selected := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	if selected == "" {
		selected = "all"
	}
	var states []models.AnomalyState
	if selected != "all" {
		parsed, err := models.ParseAnomalyStates(selected)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		states = parsed
	}

	counts, err := h.store.AnomalyStateCounts(r.Context())
	if err != nil {
		http.Error(w, "counting anomalies", http.StatusInternalServerError)
		return
	}
	anomalies, err := h.store.ListAnomalies(r.Context(), models.AnomalyFilter{States: states, Limit: anomalyListLimit})
	if err != nil {
		http.Error(w, "fetching anomalies", http.StatusInternalServerError)
		return
	}

	total := 0
	for _, n := range counts {
		total += n
	}
	matching := total
	tabs := make([]tab, 0, len(anomalyTabs))
	for _, t := range anomalyTabs {
		n := total
		if t.State != "" {
			n = counts[t.State]
		}
		tabs = append(tabs, tab{Label: t.Label, Href: "/anomalies?state=" + t.Value, Count: n, Active: t.Value == selected})
		if t.Value == selected {
			matching = n
		}
	}
	h.templates.renderPage(w, "anomalies.html", anomaliesData{
		pageBase: h.base("anomalies"), Anomalies: anomalies, Tabs: tabs, Selected: selected, Matching: matching,
	})
}

// anomalyDetailData backs templates/pages/anomaly_detail.html.
type anomalyDetailData struct {
	pageBase
	Anomaly *models.CostSnapshot
	State   models.AnomalyState
	Counts  models.EdgeCounts
	Edges   []models.BlameEdge // every candidate, best first
	History []models.CostSnapshot
}

// AnomalyDetail handles GET /anomalies/{id}: the anomaly, how it stands, and
// every candidate deploy ranked by confidence with confirm/dismiss on each.
// This is where an anomaly with only low-confidence candidates can be reviewed.
func (h *Handler) AnomalyDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid anomaly ID", http.StatusBadRequest)
		return
	}
	snap, err := h.store.CostSnapshotByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "fetching anomaly", http.StatusInternalServerError)
		return
	}
	edges, err := h.store.BlameEdgesBySnapshot(r.Context(), id)
	if err != nil {
		http.Error(w, "fetching candidates", http.StatusInternalServerError)
		return
	}

	var counts models.EdgeCounts
	for _, e := range edges {
		switch e.Status {
		case models.BlameStatusPending:
			counts.Pending++
		case models.BlameStatusResolved:
			counts.Resolved++
		case models.BlameStatusConfirmed:
			counts.Confirmed++
		case models.BlameStatusDismissed:
			counts.Dismissed++
		}
	}

	history, err := h.store.CostSnapshotsByService(r.Context(), snap.Service, snap.PeriodStart.Add(-costHistoryLookback), snap.PeriodEnd)
	if err != nil {
		slog.Warn("web: fetching cost history for the anomaly chart", "service", snap.Service, "err", err)
	}

	h.templates.renderPage(w, "anomaly_detail.html", anomalyDetailData{
		pageBase: h.base("anomalies"), Anomaly: snap, State: models.StateOf(counts), Counts: counts, Edges: edges, History: history,
	})
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
