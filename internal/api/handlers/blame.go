// Package handlers contains the HTTP handlers for the costblame REST API.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// BlameStore is the subset of store.Store required by the blame handlers.
type BlameStore interface {
	RecentBlameEdges(ctx context.Context, limit int) ([]models.BlameEdge, error)
	BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error)
	UpdateBlameEdge(ctx context.Context, edge models.BlameEdge) error
	UnblamedAnomalies(ctx context.Context) ([]models.CostSnapshot, error)
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

// ListBlame handles GET /blame
// Query params: limit (default 20)
func (h *BlameHandler) ListBlame(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	edges, err := h.store.RecentBlameEdges(r.Context(), limit)
	if err != nil {
		jsonError(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"blame_edges": edges,
		"count":       len(edges),
	})
}

// GetBlame handles GET /blame/{id}
func (h *BlameHandler) GetBlame(w http.ResponseWriter, r *http.Request) {
	id, err := uuidFromPath(r, "id")
	if err != nil {
		jsonError(w, "invalid blame edge ID", http.StatusBadRequest)
		return
	}

	edges, err := h.store.BlameEdgesBySnapshot(r.Context(), id)
	if err != nil {
		jsonError(w, "fetching blame edges", http.StatusInternalServerError)
		return
	}

	if len(edges) == 0 {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	jsonOK(w, map[string]any{"blame_edges": edges})
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

	edge := models.BlameEdge{ID: id, Status: status}
	if err := h.store.UpdateBlameEdge(r.Context(), edge); err != nil {
		jsonError(w, "updating blame edge", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"id": id, "status": status})
}

// ListAnomalies handles GET /anomalies
func (h *BlameHandler) ListAnomalies(w http.ResponseWriter, r *http.Request) {
	anomalies, err := h.store.UnblamedAnomalies(r.Context())
	if err != nil {
		jsonError(w, "fetching anomalies", http.StatusInternalServerError)
		return
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
