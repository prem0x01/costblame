package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/api/handlers"
	"github.com/prem0x01/costblame/pkg/models"
)

// fakeStore is an in-memory BlameStore.
type fakeStore struct {
	edges map[uuid.UUID]*models.BlameEdge
}

func newFakeStore(edges ...*models.BlameEdge) *fakeStore {
	m := make(map[uuid.UUID]*models.BlameEdge, len(edges))
	for _, e := range edges {
		m[e.ID] = e
	}
	return &fakeStore{edges: m}
}

func (f *fakeStore) RecentBlameEdges(ctx context.Context, limit int) ([]models.BlameEdge, error) {
	var out []models.BlameEdge
	for _, e := range f.edges {
		out = append(out, *e)
	}
	return out, nil
}

func (f *fakeStore) BlameEdgeByID(ctx context.Context, id uuid.UUID) (*models.BlameEdge, error) {
	e, ok := f.edges[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *e
	return &cp, nil
}

func (f *fakeStore) BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error) {
	var out []models.BlameEdge
	for _, e := range f.edges {
		if e.CostSnapshotID == snapshotID {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateBlameStatus(ctx context.Context, id uuid.UUID, status models.BlameStatus) error {
	e, ok := f.edges[id]
	if !ok {
		return sql.ErrNoRows
	}
	e.Status = status
	return nil
}

func (f *fakeStore) UnblamedAnomalies(ctx context.Context) ([]models.CostSnapshot, error) {
	return nil, nil
}

func (f *fakeStore) CostSnapshotsByService(ctx context.Context, service string, from, to time.Time) ([]models.CostSnapshot, error) {
	return nil, nil
}

func testEdge() *models.BlameEdge {
	return &models.BlameEdge{
		ID:              uuid.New(),
		CostSnapshotID:  uuid.New(),
		DeployEventID:   uuid.New(),
		ConfidenceScore: 0.82,
		ConfidenceFactors: []models.ConfidenceFactor{
			{Name: "temporal_proximity", Score: 1.0, Weight: 0.4, Reason: "1.5h before spike"},
		},
		Narrative: "PR #42 enabled provisioned concurrency.",
		Status:    models.BlameStatusResolved,
		CreatedAt: time.Now().UTC(),
	}
}

// mux mirrors the routes registered in api.New for the blame handler.
func mux(h *handlers.BlameHandler) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /blame/{id}", h.GetBlame)
	m.HandleFunc("POST /blame/{id}/confirm", h.ConfirmBlame)
	m.HandleFunc("POST /blame/{id}/dismiss", h.DismissBlame)
	m.HandleFunc("GET /anomalies/{id}/blame", h.ListBlameForAnomaly)
	return m
}

func TestConfirmBlame_PreservesNarrativeAndScore(t *testing.T) {
	edge := testEdge()
	store := newFakeStore(edge)
	m := mux(handlers.NewBlameHandler(store))

	req := httptest.NewRequest(http.MethodPost, "/blame/"+edge.ID.String()+"/confirm", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	got := store.edges[edge.ID]
	if got.Status != models.BlameStatusConfirmed {
		t.Errorf("Status = %q, want confirmed", got.Status)
	}
	if got.Narrative == "" || got.ConfidenceScore == 0 || len(got.ConfidenceFactors) == 0 {
		t.Errorf("confirm wiped edge data: narrative=%q score=%.2f factors=%d",
			got.Narrative, got.ConfidenceScore, len(got.ConfidenceFactors))
	}
}

func TestConfirmBlame_UnknownID_Returns404(t *testing.T) {
	m := mux(handlers.NewBlameHandler(newFakeStore()))

	req := httptest.NewRequest(http.MethodPost, "/blame/"+uuid.NewString()+"/confirm", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGetBlame_ByEdgeID(t *testing.T) {
	edge := testEdge()
	m := mux(handlers.NewBlameHandler(newFakeStore(edge)))

	req := httptest.NewRequest(http.MethodGet, "/blame/"+edge.ID.String(), nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		BlameEdge models.BlameEdge `json:"blame_edge"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body.BlameEdge.ID != edge.ID {
		t.Errorf("returned edge ID = %s, want %s", body.BlameEdge.ID, edge.ID)
	}
}

func TestListBlameForAnomaly_BySnapshotID(t *testing.T) {
	edge := testEdge()
	m := mux(handlers.NewBlameHandler(newFakeStore(edge)))

	req := httptest.NewRequest(http.MethodGet, "/anomalies/"+edge.CostSnapshotID.String()+"/blame", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		BlameEdges []models.BlameEdge `json:"blame_edges"`
		Count      int                `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body.Count != 1 || len(body.BlameEdges) != 1 {
		t.Fatalf("expected 1 edge, got count=%d len=%d", body.Count, len(body.BlameEdges))
	}
}
