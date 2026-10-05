package correlate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// fakeEngineStore is an in-memory EngineStore that records engine activity.
type fakeEngineStore struct {
	anomalies []models.CostSnapshot
	deploys   []models.DeployEvent

	deploysErr error

	savedEdges  []models.BlameEdge
	updated     []models.BlameEdge
	scoredIDs   []uuid.UUID
	blameCounts map[string]int // "author|service" → count
}

func (f *fakeEngineStore) UnscoredAnomalies(ctx context.Context) ([]models.CostSnapshot, error) {
	return f.anomalies, nil
}

func (f *fakeEngineStore) MarkAnomalyScored(ctx context.Context, id uuid.UUID) error {
	f.scoredIDs = append(f.scoredIDs, id)
	return nil
}

func (f *fakeEngineStore) DeploysBetween(ctx context.Context, from, to time.Time) ([]models.DeployEvent, error) {
	if f.deploysErr != nil {
		return nil, f.deploysErr
	}
	var out []models.DeployEvent
	for _, d := range f.deploys {
		if !d.OccurredAt.Before(from) && !d.OccurredAt.After(to) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeEngineStore) SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error {
	f.savedEdges = append(f.savedEdges, edges...)
	return nil
}

func (f *fakeEngineStore) UpdateBlameEdge(ctx context.Context, edge models.BlameEdge) error {
	f.updated = append(f.updated, edge)
	return nil
}

func (f *fakeEngineStore) ConfirmedBlameCount(ctx context.Context, author, service string) (int, error) {
	return f.blameCounts[author+"|"+service], nil
}

type fakeGenerator struct{ narrative string }

func (g *fakeGenerator) Generate(ctx context.Context, edge models.BlameEdge) (string, error) {
	return g.narrative, nil
}

type fakeNotifier struct{ sent []models.BlameGraph }

func (n *fakeNotifier) Send(ctx context.Context, graph models.BlameGraph) error {
	n.sent = append(n.sent, graph)
	return nil
}

func anomalyAt(t time.Time) models.CostSnapshot {
	return models.CostSnapshot{
		ID:          uuid.New(),
		PeriodStart: t,
		PeriodEnd:   t.Add(24 * time.Hour),
		Source:      "aws",
		Service:     "AWSLambda",
		AmountUSD:   500,
		DeltaPct:    140,
		IsAnomaly:   true,
		Granularity: models.GranularityDaily,
	}
}

func newTestEngine(store *fakeEngineStore, notifier *fakeNotifier, minScore float64) *Engine {
	return NewEngine(store, &fakeGenerator{narrative: "PR #42 did it."}, notifier, time.Minute, minScore)
}

func TestCorrelateNew_HighConfidence_SavesResolvesAndNotifies(t *testing.T) {
	spike := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	anomaly := anomalyAt(spike)

	store := &fakeEngineStore{
		anomalies: []models.CostSnapshot{anomaly},
		deploys: []models.DeployEvent{{
			// 1h before spike + exact service match ⇒ 0.4 + 0.3 + 0.1 (neutral tags) = 0.80
			ID:               uuid.New(),
			OccurredAt:       spike.Add(-1 * time.Hour),
			Source:           models.DeploySourceGitHubActions,
			PRAuthor:         "alice",
			InferredServices: []string{"AWSLambda"},
			Status:           models.DeployStatusSuccess,
		}},
	}
	notifier := &fakeNotifier{}
	engine := newTestEngine(store, notifier, 0.2)

	if err := engine.correlateNew(context.Background()); err != nil {
		t.Fatalf("correlateNew: %v", err)
	}

	if len(store.savedEdges) != 1 {
		t.Fatalf("saved edges = %d, want 1", len(store.savedEdges))
	}
	if len(store.updated) != 1 {
		t.Fatalf("updated edges = %d, want 1 (top edge resolved)", len(store.updated))
	}
	top := store.updated[0]
	if top.Status != models.BlameStatusResolved {
		t.Errorf("top edge status = %q, want resolved", top.Status)
	}
	if top.Narrative != "PR #42 did it." {
		t.Errorf("top edge narrative = %q, want generated narrative", top.Narrative)
	}
	if len(notifier.sent) != 1 {
		t.Errorf("notifications sent = %d, want 1", len(notifier.sent))
	}
	if len(store.scoredIDs) != 1 || store.scoredIDs[0] != anomaly.ID {
		t.Errorf("scored IDs = %v, want [%s]", store.scoredIDs, anomaly.ID)
	}
}

func TestCorrelateNew_NoCandidates_StillMarksScored(t *testing.T) {
	anomaly := anomalyAt(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC))
	store := &fakeEngineStore{anomalies: []models.CostSnapshot{anomaly}}
	notifier := &fakeNotifier{}
	engine := newTestEngine(store, notifier, 0.2)

	if err := engine.correlateNew(context.Background()); err != nil {
		t.Fatalf("correlateNew: %v", err)
	}

	if len(store.savedEdges) != 0 {
		t.Errorf("saved edges = %d, want 0", len(store.savedEdges))
	}
	if len(notifier.sent) != 0 {
		t.Errorf("notifications sent = %d, want 0", len(notifier.sent))
	}
	if len(store.scoredIDs) != 1 {
		t.Fatalf("anomaly with no candidates must still be marked scored, got %v", store.scoredIDs)
	}
}

func TestCorrelateNew_BelowMinScore_DiscardsButMarksScored(t *testing.T) {
	spike := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	anomaly := anomalyAt(spike)

	store := &fakeEngineStore{
		anomalies: []models.CostSnapshot{anomaly},
		deploys: []models.DeployEvent{{
			// 70h old, no service match ⇒ 0.1*0.4 + 0.5*0.2 = 0.14 < minScore 0.2
			ID:         uuid.New(),
			OccurredAt: spike.Add(-70 * time.Hour),
			Source:     models.DeploySourceGitHubActions,
			PRAuthor:   "bob",
			Status:     models.DeployStatusSuccess,
		}},
	}
	notifier := &fakeNotifier{}
	engine := newTestEngine(store, notifier, 0.2)

	if err := engine.correlateNew(context.Background()); err != nil {
		t.Fatalf("correlateNew: %v", err)
	}

	if len(store.savedEdges) != 0 {
		t.Errorf("saved edges = %d, want 0 (below threshold)", len(store.savedEdges))
	}
	if len(store.scoredIDs) != 1 {
		t.Fatalf("below-threshold anomaly must still be marked scored, got %v", store.scoredIDs)
	}
}

func TestCorrelateNew_ProcessingError_LeavesAnomalyUnscored(t *testing.T) {
	anomaly := anomalyAt(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC))
	store := &fakeEngineStore{
		anomalies:  []models.CostSnapshot{anomaly},
		deploysErr: errors.New("db locked"),
	}
	engine := newTestEngine(store, &fakeNotifier{}, 0.2)

	if err := engine.correlateNew(context.Background()); err != nil {
		t.Fatalf("correlateNew should continue past per-anomaly errors, got %v", err)
	}

	if len(store.scoredIDs) != 0 {
		t.Errorf("failed anomaly must stay unscored for retry, got %v", store.scoredIDs)
	}
}

// A deploy made during the spike day is the most likely cause of that day's
// bucketed DAILY cost, so it must be a candidate and can reach the alert tier.
func TestCorrelateNew_DeployDuringPeriod_IsBlamed(t *testing.T) {
	spike := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	anomaly := anomalyAt(spike)

	store := &fakeEngineStore{
		anomalies: []models.CostSnapshot{anomaly},
		deploys: []models.DeployEvent{{
			// 10h into a 24h period + exact service match ⇒ 0.4 + 0.3 + 0.1 = 0.80
			ID:               uuid.New(),
			OccurredAt:       spike.Add(10 * time.Hour),
			Source:           models.DeploySourceGitHubActions,
			PRAuthor:         "alice",
			InferredServices: []string{"AWSLambda"},
			Status:           models.DeployStatusSuccess,
		}},
	}
	notifier := &fakeNotifier{}
	engine := newTestEngine(store, notifier, 0.2)

	if err := engine.correlateNew(context.Background()); err != nil {
		t.Fatalf("correlateNew: %v", err)
	}
	if len(store.savedEdges) != 1 {
		t.Fatalf("saved edges = %d, want 1 (in-period deploy must be a candidate)", len(store.savedEdges))
	}
	if len(notifier.sent) != 1 {
		t.Errorf("notifications sent = %d, want 1", len(notifier.sent))
	}
}

// A deploy after the period has ended cannot have caused its cost.
func TestCorrelateNew_DeployAfterPeriod_IsNotBlamed(t *testing.T) {
	spike := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	store := &fakeEngineStore{
		anomalies: []models.CostSnapshot{anomalyAt(spike)},
		deploys: []models.DeployEvent{{
			ID:               uuid.New(),
			OccurredAt:       spike.Add(25 * time.Hour),
			InferredServices: []string{"AWSLambda"},
		}},
	}
	engine := newTestEngine(store, &fakeNotifier{}, 0.1)

	if err := engine.correlateNew(context.Background()); err != nil {
		t.Fatalf("correlateNew: %v", err)
	}
	if len(store.savedEdges) != 0 {
		t.Fatalf("saved edges = %d, want 0", len(store.savedEdges))
	}
}
