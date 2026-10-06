package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/pkg/models"
)

// anomalyDay stores an anomalous snapshot whose period starts `day` days after a
// fixed date, so tests control the ordering.
func anomalyDay(t *testing.T, s *sqlite.Store, day int, isAnomaly bool) models.CostSnapshot {
	t.Helper()
	start := edgeBase.AddDate(0, 0, day)
	snap := models.CostSnapshot{
		ID: uuid.New(), CollectedAt: start, PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour),
		Source: "aws", Service: fmt.Sprintf("Service-%d", day), AmountUSD: 100 + float64(day), PrevAmountUSD: 10,
		DeltaPct: 900, IsAnomaly: isAnomaly, AnomalyScore: 4.2, Granularity: models.GranularityDaily,
	}
	if err := s.SaveCostSnapshots(context.Background(), []models.CostSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	return snap
}

// addEdge links snap to a fresh deploy with the given status and score.
func addEdge(t *testing.T, s *sqlite.Store, snap models.CostSnapshot, status models.BlameStatus, score float64, created time.Time) models.BlameEdge {
	t.Helper()
	ctx := context.Background()
	d := models.DeployEvent{
		ID: uuid.New(), OccurredAt: snap.PeriodStart.Add(-time.Hour), Source: models.DeploySourceGitHubActions,
		Repository: "acme/payments", CommitSHA: uuid.NewString()[:8], PRAuthor: "alice", Status: models.DeployStatusSuccess,
	}
	if err := s.UpsertDeployEvent(ctx, d); err != nil {
		t.Fatal(err)
	}
	e := models.BlameEdge{
		ID: uuid.New(), CostSnapshotID: snap.ID, DeployEventID: d.ID, ConfidenceScore: score,
		Status: models.BlameStatusPending, CreatedAt: created,
	}
	if err := s.SaveBlameEdges(ctx, []models.BlameEdge{e}); err != nil {
		t.Fatal(err)
	}
	if status != models.BlameStatusPending {
		if err := s.UpdateBlameStatus(ctx, e.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	e.Status = status
	return e
}

func TestListBlameEdges_StatusFilterReachesPendingEdges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	snap := anomalyDay(t, s, 0, true)
	t0 := time.Now().UTC()
	pending := addEdge(t, s, snap, models.BlameStatusPending, 0.40, t0.Add(-4*time.Minute))
	resolved := addEdge(t, s, snap, models.BlameStatusResolved, 0.80, t0.Add(-3*time.Minute))
	confirmed := addEdge(t, s, snap, models.BlameStatusConfirmed, 0.70, t0.Add(-2*time.Minute))
	dismissed := addEdge(t, s, snap, models.BlameStatusDismissed, 0.30, t0.Add(-1*time.Minute))

	ids := func(f models.BlameEdgeFilter) []uuid.UUID {
		edges, err := s.ListBlameEdges(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]uuid.UUID, len(edges))
		for i, e := range edges {
			out[i] = e.ID
		}
		return out
	}
	eq := func(got []uuid.UUID, want ...models.BlameEdge) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i].ID {
				return false
			}
		}
		return true
	}

	if got := ids(models.BlameEdgeFilter{Statuses: []models.BlameStatus{models.BlameStatusPending}}); !eq(got, pending) {
		t.Errorf("pending filter = %v, want just the pending edge", got)
	}
	// This is what RecentBlameEdges could not do: its query excluded pending edges.
	if recent, _ := s.RecentBlameEdges(ctx, 100); len(recent) != 2 {
		t.Fatalf("setup: RecentBlameEdges should still return only resolved+confirmed, got %d", len(recent))
	}
	if got := ids(models.BlameEdgeFilter{Statuses: models.ActiveBlameStatuses}); !eq(got, confirmed, resolved, pending) {
		t.Errorf("active filter = %v, want pending+resolved+confirmed newest first and no dismissed", got)
	}
	if got := ids(models.BlameEdgeFilter{}); !eq(got, dismissed, confirmed, resolved, pending) {
		t.Errorf("no filter = %v, want every edge newest first", got)
	}
	if got := ids(models.BlameEdgeFilter{Statuses: []models.BlameStatus{models.BlameStatusDismissed}}); !eq(got, dismissed) {
		t.Errorf("dismissed filter = %v", got)
	}
	// Paging
	if got := ids(models.BlameEdgeFilter{Limit: 2}); !eq(got, dismissed, confirmed) {
		t.Errorf("limit 2 = %v", got)
	}
	if got := ids(models.BlameEdgeFilter{Limit: 2, Offset: 2}); !eq(got, resolved, pending) {
		t.Errorf("limit 2 offset 2 = %v", got)
	}
	if got := ids(models.BlameEdgeFilter{Offset: -5}); len(got) != 4 {
		t.Errorf("a negative offset must be treated as 0, got %d rows", len(got))
	}
}

func TestListAnomalies_ShowsEveryAnomalyWithItsState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	unblamed := anomalyDay(t, s, 0, true)
	candidates := anomalyDay(t, s, 1, true)
	addEdge(t, s, candidates, models.BlameStatusPending, 0.35, now)
	addEdge(t, s, candidates, models.BlameStatusPending, 0.50, now)
	resolved := anomalyDay(t, s, 2, true)
	addEdge(t, s, resolved, models.BlameStatusResolved, 0.80, now)
	addEdge(t, s, resolved, models.BlameStatusPending, 0.45, now)
	confirmed := anomalyDay(t, s, 3, true)
	addEdge(t, s, confirmed, models.BlameStatusConfirmed, 0.90, now)
	addEdge(t, s, confirmed, models.BlameStatusDismissed, 0.40, now)
	dismissed := anomalyDay(t, s, 4, true)
	addEdge(t, s, dismissed, models.BlameStatusDismissed, 0.30, now)
	notAnAnomaly := anomalyDay(t, s, 5, false)
	_ = notAnAnomaly

	got, err := s.ListAnomalies(ctx, models.AnomalyFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("anomalies = %d, want 5 (the non-anomalous snapshot is excluded)", len(got))
	}
	// Newest first.
	order := []uuid.UUID{dismissed.ID, confirmed.ID, resolved.ID, candidates.ID, unblamed.ID}
	for i, id := range order {
		if got[i].ID != id {
			t.Errorf("position %d = %s (%s), want %s", i, got[i].ID, got[i].Service, id)
		}
	}

	want := map[uuid.UUID]struct {
		state models.AnomalyState
		edges models.EdgeCounts
		top   float64
	}{
		unblamed.ID:   {models.AnomalyStateUnblamed, models.EdgeCounts{}, 0},
		candidates.ID: {models.AnomalyStateCandidates, models.EdgeCounts{Pending: 2}, 0.50},
		resolved.ID:   {models.AnomalyStateResolved, models.EdgeCounts{Resolved: 1, Pending: 1}, 0.80},
		confirmed.ID:  {models.AnomalyStateConfirmed, models.EdgeCounts{Confirmed: 1, Dismissed: 1}, 0.90},
		dismissed.ID:  {models.AnomalyStateDismissed, models.EdgeCounts{Dismissed: 1}, 0.30},
	}
	for _, a := range got {
		w := want[a.ID]
		if a.State != w.state || a.Edges != w.edges || a.TopScore != w.top {
			t.Errorf("%s: state=%q edges=%+v top=%.2f, want %q %+v %.2f", a.Service, a.State, a.Edges, a.TopScore, w.state, w.edges, w.top)
		}
		if a.AmountUSD == 0 || a.Service == "" || a.PeriodStart.IsZero() {
			t.Errorf("%s: snapshot fields not populated: %+v", a.Service, a.CostSnapshot)
		}
	}

	// The scenario from the issue: an anomaly whose only edges are pending used to
	// vanish from the unblamed list. It is now listed, in its own state.
	if unb, _ := s.UnblamedAnomalies(ctx); len(unb) != 1 || unb[0].ID != unblamed.ID {
		t.Fatalf("setup: UnblamedAnomalies should contain only the one with no edges, got %d", len(unb))
	}
	cand, _ := s.ListAnomalies(ctx, models.AnomalyFilter{States: []models.AnomalyState{models.AnomalyStateCandidates}})
	if len(cand) != 1 || cand[0].ID != candidates.ID {
		t.Errorf("candidates filter = %+v, want exactly the pending-only anomaly", cand)
	}

	// Filters combine, and paging applies after filtering.
	two, _ := s.ListAnomalies(ctx, models.AnomalyFilter{States: []models.AnomalyState{models.AnomalyStateCandidates, models.AnomalyStateUnblamed}})
	if len(two) != 2 || two[0].ID != candidates.ID || two[1].ID != unblamed.ID {
		t.Errorf("candidates+unblamed filter = %+v", two)
	}
	page, _ := s.ListAnomalies(ctx, models.AnomalyFilter{Limit: 2, Offset: 1})
	if len(page) != 2 || page[0].ID != confirmed.ID || page[1].ID != resolved.ID {
		t.Errorf("limit 2 offset 1 = %+v", page)
	}
}

// The state is computed in SQL (so filtering and paging happen in the database)
// and by models.StateOf (what the rest of the code uses). They must agree for
// every combination of edge statuses.
func TestAnomalyStateInSQLAgreesWithStateOf(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	byID := map[uuid.UUID]models.EdgeCounts{}
	statuses := []models.BlameStatus{models.BlameStatusPending, models.BlameStatusResolved, models.BlameStatusConfirmed, models.BlameStatusDismissed}
	for mask := 0; mask < 16; mask++ { // every subset of the four statuses
		snap := anomalyDay(t, s, mask, true)
		var c models.EdgeCounts
		for bit, st := range statuses {
			if mask&(1<<bit) == 0 {
				continue
			}
			addEdge(t, s, snap, st, 0.5, now)
			switch st {
			case models.BlameStatusPending:
				c.Pending++
			case models.BlameStatusResolved:
				c.Resolved++
			case models.BlameStatusConfirmed:
				c.Confirmed++
			case models.BlameStatusDismissed:
				c.Dismissed++
			}
		}
		byID[snap.ID] = c
	}

	all, err := s.ListAnomalies(ctx, models.AnomalyFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 16 {
		t.Fatalf("anomalies = %d, want 16", len(all))
	}
	for _, a := range all {
		if want := models.StateOf(byID[a.ID]); a.State != want {
			t.Errorf("counts %+v: SQL says %q, StateOf says %q", byID[a.ID], a.State, want)
		}
		if a.Edges != byID[a.ID] {
			t.Errorf("counts = %+v, want %+v", a.Edges, byID[a.ID])
		}
	}
}

// Dashboard figures used to be computed from the latest 100 edges and were
// wrong beyond that. The counts now come from the database.
func TestCounts_AreExactBeyondAHundredRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	snap := anomalyDay(t, s, 0, true)
	now := time.Now().UTC()
	for i := 0; i < 130; i++ {
		st := models.BlameStatusPending
		if i%10 == 0 {
			st = models.BlameStatusConfirmed
		}
		addEdge(t, s, snap, st, 0.5, now)
	}
	for i := 1; i <= 120; i++ { // 120 more anomalies, none with edges
		anomalyDay(t, s, i, true)
	}

	edgeCounts, err := s.BlameStatusCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if edgeCounts[models.BlameStatusPending] != 117 || edgeCounts[models.BlameStatusConfirmed] != 13 {
		t.Errorf("edge counts = %v, want 117 pending and 13 confirmed", edgeCounts)
	}
	anomalyCounts, err := s.AnomalyStateCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if anomalyCounts[models.AnomalyStateUnblamed] != 120 || anomalyCounts[models.AnomalyStateConfirmed] != 1 {
		t.Errorf("anomaly counts = %v, want 120 unblamed and 1 confirmed", anomalyCounts)
	}
}

func TestCostSnapshotByID(t *testing.T) {
	s := newTestStore(t)
	snap := anomalyDay(t, s, 3, true)

	got, err := s.CostSnapshotByID(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != snap.ID || got.Service != snap.Service || got.AmountUSD != snap.AmountUSD {
		t.Errorf("got %+v, want %+v", got, snap)
	}
	if _, err := s.CostSnapshotByID(context.Background(), uuid.New()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown ID: err = %v, want sql.ErrNoRows", err)
	}
}
