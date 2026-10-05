package sqlite_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/pkg/models"
)

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	f, err := os.CreateTemp("", "costblame-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	store, err := sqlite.New(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func TestSaveCostSnapshots_ThenUnblamedAnomalies(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	snaps := []models.CostSnapshot{
		{
			ID:            uuid.New(),
			CollectedAt:   time.Now().UTC(),
			PeriodStart:   time.Now().UTC().Add(-24 * time.Hour),
			PeriodEnd:     time.Now().UTC(),
			Source:        "aws",
			Service:       "AmazonEC2",
			Region:        "us-east-1",
			AmountUSD:     150.0,
			PrevAmountUSD: 100.0,
			DeltaUSD:      50.0,
			DeltaPct:      50.0,
			IsAnomaly:     true,
			AnomalyScore:  2.5,
			Granularity:   models.GranularityDaily,
		},
	}

	if err := store.SaveCostSnapshots(ctx, snaps); err != nil {
		t.Fatalf("SaveCostSnapshots: %v", err)
	}

	anomalies, err := store.UnblamedAnomalies(ctx)
	if err != nil {
		t.Fatalf("UnblamedAnomalies: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("expected 1 anomaly, got %d", len(anomalies))
	}
	if anomalies[0].Service != "AmazonEC2" {
		t.Errorf("service = %q, want AmazonEC2", anomalies[0].Service)
	}
}

func TestSaveCostSnapshots_RePollUpsertsSamePeriod(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	periodStart := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.Add(24 * time.Hour)

	snap := models.CostSnapshot{
		ID:          models.SnapshotID("aws", "AmazonEC2", periodStart, periodEnd, models.GranularityDaily),
		CollectedAt: time.Now().UTC(),
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		Source:      "aws",
		Service:     "AmazonEC2",
		AmountUSD:   100.0,
		IsAnomaly:   false,
		Granularity: models.GranularityDaily,
	}
	if err := store.SaveCostSnapshots(ctx, []models.CostSnapshot{snap}); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Re-poll of the same period: same natural key ⇒ same ID, updated amounts.
	repoll := snap
	repoll.ID = models.SnapshotID("aws", "AmazonEC2", periodStart, periodEnd, models.GranularityDaily)
	repoll.AmountUSD = 250.0
	repoll.IsAnomaly = true
	repoll.AnomalyScore = 3.1
	if err := store.SaveCostSnapshots(ctx, []models.CostSnapshot{repoll}); err != nil {
		t.Fatalf("re-poll save: %v", err)
	}

	anomalies, err := store.UnblamedAnomalies(ctx)
	if err != nil {
		t.Fatalf("UnblamedAnomalies: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("expected exactly 1 row after re-poll, got %d", len(anomalies))
	}
	if anomalies[0].AmountUSD != 250.0 {
		t.Errorf("AmountUSD = %.1f, want 250.0 (re-poll should refresh amounts)", anomalies[0].AmountUSD)
	}
}

func TestUpsertDeployEvent_TwoPhase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id := uuid.New()
	now := time.Now().UTC()

	// Phase 1: initial sparse record (no PR info yet).
	ev1 := models.DeployEvent{
		ID:         id,
		OccurredAt: now,
		Source:     models.DeploySourceGitHubActions,
		Repository: "myorg/myrepo",
		Branch:     "main",
		CommitSHA:  "abc123",
		Status:     models.DeployStatusSuccess,
	}
	if err := store.UpsertDeployEvent(ctx, ev1); err != nil {
		t.Fatalf("phase 1 upsert: %v", err)
	}

	// Phase 2: enriched record with PR info.
	ev2 := ev1
	ev2.PRNumber = 42
	ev2.PRTitle = "feat: enable caching"
	ev2.PRAuthor = "alice"
	ev2.ChangedFiles = []string{"lambda/handler.go", "go.mod"}
	ev2.InferredServices = []string{"AWSLambda"}

	if err := store.UpsertDeployEvent(ctx, ev2); err != nil {
		t.Fatalf("phase 2 upsert: %v", err)
	}

	events, err := store.DeploysBetween(ctx, now.Add(-1*time.Hour), now.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("DeploysBetween: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one event")
	}

	var found *models.DeployEvent
	for i := range events {
		if events[i].ID == id {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("event %s not found", id)
	}
	if found.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", found.PRNumber)
	}
	if found.PRAuthor != "alice" {
		t.Errorf("PRAuthor = %q, want alice", found.PRAuthor)
	}
	if len(found.InferredServices) == 0 || found.InferredServices[0] != "AWSLambda" {
		t.Errorf("InferredServices = %v, want [AWSLambda]", found.InferredServices)
	}
}

func TestRecentBlameEdges_Empty(t *testing.T) {
	store := newTestStore(t)
	edges, err := store.RecentBlameEdges(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentBlameEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(edges))
	}
}

func TestSaveBlameEdges_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	snapID := uuid.New()
	deployID := uuid.New()

	// Seed snapshot and deploy event first (FK constraints).
	snaps := []models.CostSnapshot{{
		ID:          snapID,
		CollectedAt: time.Now().UTC(),
		PeriodStart: time.Now().UTC().Add(-1 * time.Hour),
		PeriodEnd:   time.Now().UTC(),
		Source:      "aws",
		Service:     "AWSLambda",
		IsAnomaly:   true,
		Granularity: models.GranularityHourly,
	}}
	if err := store.SaveCostSnapshots(ctx, snaps); err != nil {
		t.Fatalf("seed snapshots: %v", err)
	}
	if err := store.UpsertDeployEvent(ctx, models.DeployEvent{
		ID:         deployID,
		OccurredAt: time.Now().UTC().Add(-30 * time.Minute),
		Source:     models.DeploySourceGitHubActions,
		Repository: "myorg/myrepo",
		PRAuthor:   "bob",
		Status:     models.DeployStatusSuccess,
	}); err != nil {
		t.Fatalf("seed deploy event: %v", err)
	}

	edge := models.BlameEdge{
		ID:              uuid.New(),
		CostSnapshotID:  snapID,
		DeployEventID:   deployID,
		ConfidenceScore: 0.88,
		ConfidenceFactors: []models.ConfidenceFactor{
			{Name: "temporal", Score: 1.0, Weight: 0.40, Reason: "within 30m"},
		},
		Narrative: "Test narrative.",
		Status:    models.BlameStatusConfirmed,
		CreatedAt: time.Now().UTC(),
	}

	if err := store.SaveBlameEdges(ctx, []models.BlameEdge{edge}); err != nil {
		t.Fatalf("SaveBlameEdges: %v", err)
	}

	edges, err := store.RecentBlameEdges(ctx, 5)
	if err != nil {
		t.Fatalf("RecentBlameEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].ConfidenceScore != 0.88 {
		t.Errorf("ConfidenceScore = %.2f, want 0.88", edges[0].ConfidenceScore)
	}
	if edges[0].CostSnapshot == nil || edges[0].CostSnapshot.Service != "AWSLambda" {
		t.Errorf("CostSnapshot not hydrated with service AWSLambda: %+v", edges[0].CostSnapshot)
	}
	if edges[0].DeployEvent == nil || edges[0].DeployEvent.PRAuthor != "bob" {
		t.Errorf("DeployEvent not hydrated with PRAuthor bob: %+v", edges[0].DeployEvent)
	}
}
