package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/api/handlers"
	"github.com/prem0x01/costblame/pkg/models"
)

// fakeStore is an in-memory BlameStore.
type fakeStore struct {
	edges     map[uuid.UUID]*models.BlameEdge
	anomalies []models.AnomalySummary
}

func newFakeStore(edges ...*models.BlameEdge) *fakeStore {
	m := make(map[uuid.UUID]*models.BlameEdge, len(edges))
	for _, e := range edges {
		m[e.ID] = e
	}
	return &fakeStore{edges: m}
}

// ListBlameEdges filters by status, newest first, and pages like the real store.
func (f *fakeStore) ListBlameEdges(ctx context.Context, flt models.BlameEdgeFilter) ([]models.BlameEdge, error) {
	var out []models.BlameEdge
	for _, e := range f.edges {
		if len(flt.Statuses) > 0 && !containsStatus(flt.Statuses, e.Status) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if flt.Offset > 0 {
		if flt.Offset >= len(out) {
			return nil, nil
		}
		out = out[flt.Offset:]
	}
	if flt.Limit > 0 && flt.Limit < len(out) {
		out = out[:flt.Limit]
	}
	return out, nil
}

func containsStatus(list []models.BlameStatus, s models.BlameStatus) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeStore) BlameStatusCounts(ctx context.Context) (map[models.BlameStatus]int, error) {
	out := map[models.BlameStatus]int{}
	for _, e := range f.edges {
		out[e.Status]++
	}
	return out, nil
}

func (f *fakeStore) ListAnomalies(ctx context.Context, flt models.AnomalyFilter) ([]models.AnomalySummary, error) {
	var out []models.AnomalySummary
	for _, a := range f.anomalies {
		if len(flt.States) > 0 {
			ok := false
			for _, st := range flt.States {
				ok = ok || st == a.State
			}
			if !ok {
				continue
			}
		}
		out = append(out, a)
	}
	if flt.Offset > 0 {
		if flt.Offset >= len(out) {
			return nil, nil
		}
		out = out[flt.Offset:]
	}
	if flt.Limit > 0 && flt.Limit < len(out) {
		out = out[:flt.Limit]
	}
	return out, nil
}

func (f *fakeStore) AnomalyStateCounts(ctx context.Context) (map[models.AnomalyState]int, error) {
	out := map[models.AnomalyState]int{}
	for _, a := range f.anomalies {
		out[a.State]++
	}
	return out, nil
}

func (f *fakeStore) CostSnapshotByID(ctx context.Context, id uuid.UUID) (*models.CostSnapshot, error) {
	for _, a := range f.anomalies {
		if a.ID == id {
			cp := a.CostSnapshot
			return &cp, nil
		}
	}
	return nil, sql.ErrNoRows
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
	m.HandleFunc("GET /blame", h.ListBlame)
	m.HandleFunc("GET /anomalies", h.ListAnomalies)
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

// ---- listing and filtering (the pending-edges fix) ----

func edgeWith(status models.BlameStatus, age time.Duration) *models.BlameEdge {
	e := testEdge()
	e.Status = status
	e.CreatedAt = time.Now().UTC().Add(-age)
	return e
}

func getJSON(t *testing.T, m http.Handler, target string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func statusesOf(body map[string]any) []string {
	var out []string
	for _, e := range body["blame_edges"].([]any) {
		out = append(out, e.(map[string]any)["status"].(string))
	}
	return out
}

func TestListBlame_DefaultsToActiveEdgesIncludingPending(t *testing.T) {
	m := mux(handlers.NewBlameHandler(newFakeStore(
		edgeWith(models.BlameStatusPending, 4*time.Minute),
		edgeWith(models.BlameStatusResolved, 3*time.Minute),
		edgeWith(models.BlameStatusConfirmed, 2*time.Minute),
		edgeWith(models.BlameStatusDismissed, 1*time.Minute),
	)))

	code, body := getJSON(t, m, "/blame")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got := statusesOf(body)
	if len(got) != 3 || got[0] != "confirmed" || got[1] != "resolved" || got[2] != "pending" {
		t.Errorf("default list = %v, want confirmed, resolved, pending (newest first) and no dismissed", got)
	}
	if body["count"].(float64) != 3 {
		t.Errorf("count = %v, want 3", body["count"])
	}
}

func TestListBlame_StatusFilter(t *testing.T) {
	m := mux(handlers.NewBlameHandler(newFakeStore(
		edgeWith(models.BlameStatusPending, 4*time.Minute),
		edgeWith(models.BlameStatusResolved, 3*time.Minute),
		edgeWith(models.BlameStatusDismissed, 1*time.Minute),
	)))

	for target, want := range map[string][]string{
		"/blame?status=pending":              {"pending"},
		"/blame?status=dismissed":            {"dismissed"},
		"/blame?status=pending,resolved":     {"resolved", "pending"},
		"/blame?status=ALL":                  {"dismissed", "resolved", "pending"},
		"/blame?status=active":               {"resolved", "pending"}, // the same word the UI uses
		"/blame?status=%20Pending%20":        {"pending"},             // case and spaces are forgiven
		"/blame?status=pending&limit=1":      {"pending"},
		"/blame?status=all&limit=1&offset=1": {"resolved"},
		"/blame?status=all&offset=99":        {},
	} {
		code, body := getJSON(t, m, target)
		if code != http.StatusOK {
			t.Errorf("%s: status = %d", target, code)
			continue
		}
		got := statusesOf(body)
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", target, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", target, got, want)
			}
		}
	}
}

func TestListBlame_RejectsUnknownStatus(t *testing.T) {
	m := mux(handlers.NewBlameHandler(newFakeStore()))
	code, body := getJSON(t, m, "/blame?status=pending,bogus")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "bogus") {
		t.Errorf("error = %q, want it to name the bad value", msg)
	}
}

func summary(state models.AnomalyState, service string, edges models.EdgeCounts) models.AnomalySummary {
	return models.AnomalySummary{
		CostSnapshot: models.CostSnapshot{ID: uuid.New(), Service: service, AmountUSD: 500, DeltaPct: 140, IsAnomaly: true},
		State:        state, Edges: edges, TopScore: 0.4,
	}
}

func TestListAnomalies_ReturnsEveryStateWithCounts(t *testing.T) {
	store := newFakeStore()
	store.anomalies = []models.AnomalySummary{
		summary(models.AnomalyStateUnblamed, "AWS Lambda", models.EdgeCounts{}),
		summary(models.AnomalyStateCandidates, "Amazon S3", models.EdgeCounts{Pending: 2}),
		summary(models.AnomalyStateResolved, "Amazon RDS", models.EdgeCounts{Resolved: 1}),
	}
	m := mux(handlers.NewBlameHandler(store))

	code, body := getJSON(t, m, "/anomalies")
	if code != http.StatusOK || body["count"].(float64) != 3 {
		t.Fatalf("status=%d body=%v, want all 3 anomalies (a pending-only one used to be missing)", code, body)
	}
	first := body["anomalies"].([]any)[1].(map[string]any)
	if first["state"] != "candidates" || first["service"] != "Amazon S3" { // snapshot fields stay at the top level
		t.Errorf("anomaly JSON = %v", first)
	}
	if counts := first["edge_counts"].(map[string]any); counts["pending"].(float64) != 2 {
		t.Errorf("edge_counts = %v", counts)
	}

	_, body = getJSON(t, m, "/anomalies?state=candidates")
	if body["count"].(float64) != 1 {
		t.Errorf("state=candidates returned %v anomalies, want 1", body["count"])
	}
	_, body = getJSON(t, m, "/anomalies?state=unblamed,resolved")
	if body["count"].(float64) != 2 {
		t.Errorf("state=unblamed,resolved returned %v anomalies, want 2", body["count"])
	}
	if code, _ = getJSON(t, m, "/anomalies?state=pending"); code != http.StatusBadRequest {
		t.Errorf("an edge status is not an anomaly state: status = %d, want 400", code)
	}
}

// Clients iterate these lists; an empty page must be [] and never null.
func TestListEndpoints_EmptyResultsAreArraysNotNull(t *testing.T) {
	m := mux(handlers.NewBlameHandler(newFakeStore()))
	for target, key := range map[string]string{"/blame": "blame_edges", "/anomalies": "anomalies"} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if !strings.Contains(rec.Body.String(), `"`+key+`":[]`) {
			t.Errorf("%s with no data = %s, want %q to be []", target, strings.TrimSpace(rec.Body.String()), key)
		}
	}
}
