package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/pkg/models"
)

var edgeBase = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)

// seedPair stores one anomaly snapshot and one deploy, returning both.
func seedPair(t *testing.T, s *sqlite.Store) (models.CostSnapshot, models.DeployEvent) {
	t.Helper()
	ctx := context.Background()
	snap := models.CostSnapshot{
		ID: uuid.New(), CollectedAt: edgeBase, PeriodStart: edgeBase, PeriodEnd: edgeBase.Add(24 * time.Hour),
		Source: "aws", Service: "AWSLambda", AmountUSD: 500, IsAnomaly: true, Granularity: models.GranularityDaily,
	}
	if err := s.SaveCostSnapshots(ctx, []models.CostSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	d := models.DeployEvent{
		ID: uuid.New(), OccurredAt: edgeBase.Add(-time.Hour), Source: models.DeploySourceGitHubActions,
		Repository: "acme/payments", CommitSHA: "abc123", PRAuthor: "alice", Status: models.DeployStatusSuccess,
	}
	if err := s.UpsertDeployEvent(ctx, d); err != nil {
		t.Fatal(err)
	}
	return snap, d
}

func edge(snap models.CostSnapshot, d models.DeployEvent, score float64, status models.BlameStatus, narrative string) models.BlameEdge {
	return models.BlameEdge{
		ID: uuid.New(), CostSnapshotID: snap.ID, DeployEventID: d.ID,
		ConfidenceScore: score, Status: status, Narrative: narrative, CreatedAt: time.Now().UTC(),
	}
}

func edgesOf(t *testing.T, s *sqlite.Store, snap models.CostSnapshot) []models.BlameEdge {
	t.Helper()
	e, err := s.BlameEdgesBySnapshot(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSaveBlameEdges_OneEdgePerPairEvenWithDifferentIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	snap, d := seedPair(t, s)

	first := edge(snap, d, 0.50, models.BlameStatusPending, "")
	second := edge(snap, d, 0.60, models.BlameStatusPending, "") // a retry: new random ID, same pair
	for _, e := range []models.BlameEdge{first, second} {
		if err := s.SaveBlameEdges(ctx, []models.BlameEdge{e}); err != nil {
			t.Fatal(err)
		}
	}

	got := edgesOf(t, s, snap)
	if len(got) != 1 {
		t.Fatalf("rows for one (anomaly, deploy) pair = %d, want 1", len(got))
	}
	if got[0].ID != first.ID {
		t.Errorf("edge ID changed on update: %s, want the original %s", got[0].ID, first.ID)
	}
	if got[0].ConfidenceScore != 0.60 {
		t.Errorf("a pending edge should be refreshed with the newer score, got %v", got[0].ConfidenceScore)
	}
}

func TestSaveBlameEdges_PromotesPendingToResolvedWithNarrative(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	snap, d := seedPair(t, s)

	if err := s.SaveBlameEdges(ctx, []models.BlameEdge{edge(snap, d, 0.5, models.BlameStatusPending, "")}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBlameEdges(ctx, []models.BlameEdge{edge(snap, d, 0.8, models.BlameStatusResolved, "PR #42 did it.")}); err != nil {
		t.Fatal(err)
	}

	got := edgesOf(t, s, snap)
	if len(got) != 1 || got[0].Status != models.BlameStatusResolved || got[0].Narrative != "PR #42 did it." || got[0].ConfidenceScore != 0.8 {
		t.Errorf("promotion not applied: %+v", got)
	}
}

// Once an edge has moved past pending, later scoring passes must not touch it.
func TestSaveBlameEdges_NeverOverwritesReviewedOrAlertedEdges(t *testing.T) {
	for _, status := range []models.BlameStatus{models.BlameStatusResolved, models.BlameStatusConfirmed, models.BlameStatusDismissed} {
		t.Run(string(status), func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			snap, d := seedPair(t, s)

			orig := edge(snap, d, 0.80, models.BlameStatusResolved, "original narrative")
			if err := s.SaveBlameEdges(ctx, []models.BlameEdge{orig}); err != nil {
				t.Fatal(err)
			}
			if status != models.BlameStatusResolved {
				if err := s.UpdateBlameStatus(ctx, orig.ID, status); err != nil {
					t.Fatal(err)
				}
			}

			// A later pass tries to downgrade it and replace its narrative.
			if err := s.SaveBlameEdges(ctx, []models.BlameEdge{edge(snap, d, 0.30, models.BlameStatusPending, "")}); err != nil {
				t.Fatal(err)
			}

			got := edgesOf(t, s, snap)
			if len(got) != 1 || got[0].Status != status || got[0].ConfidenceScore != 0.80 || got[0].Narrative != "original narrative" {
				t.Errorf("edge was overwritten: %+v", got)
			}
		})
	}
}

// A batch is all-or-nothing: one bad edge must not leave the others behind.
func TestSaveBlameEdges_IsAtomic(t *testing.T) {
	s := newTestStore(t)
	snap, d := seedPair(t, s)

	good := edge(snap, d, 0.7, models.BlameStatusPending, "")
	bad := edge(snap, d, 0.7, models.BlameStatusPending, "")
	bad.CostSnapshotID = uuid.New() // violates the foreign key
	bad.DeployEventID = uuid.New()

	if err := s.SaveBlameEdges(context.Background(), []models.BlameEdge{good, bad}); err == nil {
		t.Fatal("expected the foreign-key violation to fail the batch")
	}
	if n := len(edgesOf(t, s, snap)); n != 0 {
		t.Errorf("a failed batch left %d edge(s) behind", n)
	}
}

func TestUnalertedEdges_IsTheAlertOutbox(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	snap, d := seedPair(t, s)
	resolved := edge(snap, d, 0.8, models.BlameStatusResolved, "n")
	if err := s.SaveBlameEdges(ctx, []models.BlameEdge{resolved}); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-24 * time.Hour)

	got, err := s.UnalertedEdges(ctx, long)
	if err != nil || len(got) != 1 || got[0].ID != resolved.ID {
		t.Fatalf("outbox = %v, %v; want the resolved edge", got, err)
	}
	if got[0].CostSnapshot == nil || got[0].DeployEvent == nil {
		t.Error("outbox edges must come with their snapshot and deploy for the notifier")
	}

	// A cutoff after the edge was created excludes it.
	if stale, _ := s.UnalertedEdges(ctx, time.Now().Add(time.Hour)); len(stale) != 0 {
		t.Errorf("edge older than the cutoff is still in the outbox: %v", stale)
	}

	if err := s.MarkEdgeAlerted(ctx, resolved.ID); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.UnalertedEdges(ctx, long); len(after) != 0 {
		t.Errorf("a delivered alert is still in the outbox: %v", after)
	}
}

func TestUnalertedEdges_OnlyResolvedEdges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	snap, d := seedPair(t, s)
	if err := s.SaveBlameEdges(ctx, []models.BlameEdge{edge(snap, d, 0.5, models.BlameStatusPending, "")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UnalertedEdges(ctx, time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Errorf("a pending edge is not alertable: %v", got)
	}
}

func TestRecentlyScoredAnomalies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	scored, _ := seedPair(t, s)
	unscored, _ := seedPair(t, s)
	if err := s.MarkAnomalyScored(ctx, scored.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentlyScoredAnomalies(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != scored.ID {
		ids := []uuid.UUID{}
		for _, g := range got {
			ids = append(ids, g.ID)
		}
		t.Fatalf("got %v, want only the scored anomaly %s (not %s)", ids, scored.ID, unscored.ID)
	}

	if old, _ := s.RecentlyScoredAnomalies(ctx, time.Now().Add(time.Hour)); len(old) != 0 {
		t.Errorf("an anomaly scored before the window is still returned: %v", old)
	}
}
