// Package handlers contains the HTTP handlers for the costblame REST API.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// BlameStore is the subset of store.Store required by the blame handlers.
type BlameStore interface {
	ListBlameEdges(ctx context.Context, f models.BlameEdgeFilter) ([]models.BlameEdge, error)
	BlameStatusCounts(ctx context.Context) (map[models.BlameStatus]int, error)
	BlameEdgeByID(ctx context.Context, id uuid.UUID) (*models.BlameEdge, error)
	BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error)
	UpdateBlameStatus(ctx context.Context, id uuid.UUID, status models.BlameStatus) error
	ListAnomalies(ctx context.Context, f models.AnomalyFilter) ([]models.AnomalySummary, error)
	AnomalyStateCounts(ctx context.Context) (map[models.AnomalyState]int, error)
	CostSnapshotByID(ctx context.Context, id uuid.UUID) (*models.CostSnapshot, error)
	CostSnapshotsByService(ctx context.Context, service string, from, to time.Time) ([]models.CostSnapshot, error)
}

// BlameHandler provides blame-related REST endpoints.
type BlameHandler struct {
	store BlameStore
}

// NewBlameHandler creates a BlameHandler.
func NewBlameHandler(store BlameStore) *BlameHandler {
	return &BlameHandler{store: store}
}

// ListBlame handles GET /api/blame.
//
// Query params:
//   - status: comma-separated statuses (pending, resolved, confirmed, dismissed)
//     or "all". Default: pending, resolved and confirmed. Pending edges are the
//     medium-confidence candidates most anomalies end up with, so they are
//     included by default; dismissed ones are hidden unless asked for.
//   - limit: page size (default 20, at most 200); offset: rows to skip.
func (h *BlameHandler) ListBlame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	statuses := models.ActiveBlameStatuses
	if raw := q.Get("status"); raw != "" {
		parsed, err := models.ParseBlameStatuses(raw)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(parsed) > 0 {
			statuses = parsed
		}
	}
	limit, offset := pageParams(q.Get("limit"), q.Get("offset"), 20)

	edges, err := h.store.ListBlameEdges(r.Context(), models.BlameEdgeFilter{Statuses: statuses, Limit: limit, Offset: offset})
	if err != nil {
		jsonError(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	if edges == nil {
		edges = []models.BlameEdge{} // an empty page is [], not null
	}
	jsonOK(w, map[string]any{
		"blame_edges": edges,
		"count":       len(edges),
		"statuses":    statuses,
	})
}

// pageParams reads limit and offset query values, falling back to the given
// default limit (limit must be 1..200) and a zero offset.
func pageParams(limitRaw, offsetRaw string, defaultLimit int) (limit, offset int) {
	limit = defaultLimit
	if n, err := strconv.Atoi(limitRaw); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	if n, err := strconv.Atoi(offsetRaw); err == nil && n > 0 {
		offset = n
	}
	return limit, offset
}

// GetBlame handles GET /blame/{id} — {id} is a blame edge ID, the same ID
// returned by GET /blame and accepted by the confirm/dismiss endpoints.
func (h *BlameHandler) GetBlame(w http.ResponseWriter, r *http.Request) {
	id, err := uuidFromPath(r, "id")
	if err != nil {
		jsonError(w, "invalid blame edge ID", http.StatusBadRequest)
		return
	}

	edge, err := h.store.BlameEdgeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		jsonError(w, "fetching blame edge", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"blame_edge": edge})
}

// ListBlameForAnomaly handles GET /anomalies/{id}/blame — {id} is a cost
// snapshot ID; returns every candidate edge for that anomaly, best first.
func (h *BlameHandler) ListBlameForAnomaly(w http.ResponseWriter, r *http.Request) {
	id, err := uuidFromPath(r, "id")
	if err != nil {
		jsonError(w, "invalid anomaly ID", http.StatusBadRequest)
		return
	}

	edges, err := h.store.BlameEdgesBySnapshot(r.Context(), id)
	if err != nil {
		jsonError(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"blame_edges": edges, "count": len(edges)})
}

// ConfirmBlame handles POST /blame/{id}/confirm
func (h *BlameHandler) ConfirmBlame(w http.ResponseWriter, r *http.Request) {
	h.updateStatus(w, r, models.BlameStatusConfirmed)
}

// DismissBlame handles POST /blame/{id}/dismiss
func (h *BlameHandler) DismissBlame(w http.ResponseWriter, r *http.Request) {
	h.updateStatus(w, r, models.BlameStatusDismissed)
}

func (h *BlameHandler) updateStatus(w http.ResponseWriter, r *http.Request, status models.BlameStatus) {
	id, err := uuidFromPath(r, "id")
	if err != nil {
		jsonError(w, "invalid blame edge ID", http.StatusBadRequest)
		return
	}

	if err := h.store.UpdateBlameStatus(r.Context(), id, status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		jsonError(w, "updating blame edge", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"id": id, "status": status})
}

// ListAnomalies handles GET /api/anomalies.
//
// Every anomaly is returned with its state (unblamed, candidates, resolved,
// confirmed or dismissed), its candidate counts and its best score. An anomaly
// whose only edges are low-confidence candidates is "candidates", not hidden.
//
// Query params: state (comma-separated, or "all"; default all), limit (default
// 50, at most 200) and offset.
func (h *BlameHandler) ListAnomalies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var states []models.AnomalyState
	if raw := q.Get("state"); raw != "" {
		parsed, err := models.ParseAnomalyStates(raw)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		states = parsed
	}
	limit, offset := pageParams(q.Get("limit"), q.Get("offset"), 50)

	anomalies, err := h.store.ListAnomalies(r.Context(), models.AnomalyFilter{States: states, Limit: limit, Offset: offset})
	if err != nil {
		jsonError(w, "fetching anomalies", http.StatusInternalServerError)
		return
	}
	if anomalies == nil {
		anomalies = []models.AnomalySummary{}
	}
	jsonOK(w, map[string]any{"anomalies": anomalies, "count": len(anomalies)})
}

// --- helpers ---

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// uuidFromPath extracts a UUID path segment added by the router under `key`.
// Routers that use PathValue (Go 1.22 stdlib) are supported directly.
func uuidFromPath(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(r.PathValue(key))
}
