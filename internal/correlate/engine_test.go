package correlate

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/pkg/models"
)

// These tests run the engine against a real SQLite store, so they exercise the
// SQL behind idempotent saves, the alert outbox and re-scoring, not a fake that
// merely agrees with the engine.

var spike = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)

// hookStore wraps the real store with failure injection and call counters.
type hookStore struct {
	*sqlite.Store
	deploysErr     error
	failMarkScored int // number of upcoming MarkAnomalyScored calls to fail
	saves          int
	historyQueries int // ConfirmedBlameCount round trips
}

func (h *hookStore) ConfirmedBlameCount(ctx context.Context, author, service string) (int, error) {
	h.historyQueries++
	return h.Store.ConfirmedBlameCount(ctx, author, service)
}

func (h *hookStore) DeploysBetween(ctx context.Context, from, to time.Time) ([]models.DeployEvent, error) {
	if h.deploysErr != nil {
		return nil, h.deploysErr
	}
	return h.Store.DeploysBetween(ctx, from, to)
}

func (h *hookStore) MarkAnomalyScored(ctx context.Context, id uuid.UUID) error {
	if h.failMarkScored > 0 {
		h.failMarkScored--
		return errors.New("injected: could not mark anomaly scored")
	}
	return h.Store.MarkAnomalyScored(ctx, id)
}

func (h *hookStore) SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error {
	h.saves++
	return h.Store.SaveBlameEdges(ctx, edges)
}

type fakeGenerator struct{ narrative string }

func (g *fakeGenerator) Generate(ctx context.Context, edge models.BlameEdge) (string, error) {
	return g.narrative, nil
}

// fakeNotifier records deliveries; while fail is set it rejects every send.
type fakeNotifier struct {
	sent     []models.BlameGraph
	attempts int
	fail     bool
}

func (n *fakeNotifier) Send(ctx context.Context, graph models.BlameGraph) error {
	n.attempts++
	if n.fail {
		return errors.New("injected: notifier unavailable")
	}
	n.sent = append(n.sent, graph)
	return nil
}

type env struct {
	t        *testing.T
	store    *hookStore
	notifier *fakeNotifier
	engine   *Engine
}

func newEnv(t *testing.T, minScore float64) *env {
	t.Helper()
	f, err := os.CreateTemp("", "costblame-engine-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	real, err := sqlite.New(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { real.Close() })
	if err := real.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, store: &hookStore{Store: real}, notifier: &fakeNotifier{}}
	e.engine = NewEngine(e.store, &fakeGenerator{narrative: "PR #42 did it."}, e.notifier, time.Minute, minScore)
	// Most tests run cycles back to back and expect immediate re-scoring; the
	// gap between re-scoring passes has its own test.
	e.engine.rescoreGap = 0
	return e
}

func anomalyAt(t time.Time) models.CostSnapshot {
	return models.CostSnapshot{
		ID:          uuid.New(),
		PeriodStart: t,
		PeriodEnd:   t.Add(24 * time.Hour),
		CollectedAt: t,
		Source:      "aws",
		Service:     "AWSLambda",
		AmountUSD:   500,
		DeltaPct:    140,
		IsAnomaly:   true,
		Granularity: models.GranularityDaily,
	}
}

func deployAt(at time.Time, services ...string) models.DeployEvent {
	return models.DeployEvent{
		ID:               uuid.New(),
		OccurredAt:       at,
		Source:           models.DeploySourceGitHubActions,
		Repository:       "acme/payments",
		CommitSHA:        uuid.NewString()[:8],
		PRNumber:         42,
		PRAuthor:         "alice",
		InferredServices: services,
		Environment:      "prod",
		Status:           models.DeployStatusSuccess,
	}
}

func (e *env) seedAnomaly(a models.CostSnapshot) {
	e.t.Helper()
	if err := e.store.SaveCostSnapshots(context.Background(), []models.CostSnapshot{a}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) seedDeploy(d models.DeployEvent) {
	e.t.Helper()
	if err := e.store.UpsertDeployEvent(context.Background(), d); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) cycle() {
	e.t.Helper()
	if err := e.engine.correlateNew(context.Background()); err != nil {
		e.t.Fatalf("correlateNew: %v", err)
	}
}

func (e *env) edges(a models.CostSnapshot) []models.BlameEdge {
	e.t.Helper()
	edges, err := e.store.BlameEdgesBySnapshot(context.Background(), a.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return edges
}

func (e *env) unscored() int {
	e.t.Helper()
	a, err := e.store.UnscoredAnomalies(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return len(a)
}

func (e *env) outbox() []models.BlameEdge {
	e.t.Helper()
	edges, err := e.store.UnalertedEdges(context.Background(), time.Now().Add(-100*24*time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	return edges
}

func TestCorrelateNew_HighConfidence_SavesResolvesAndNotifies(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	// 1h before the period + exact service match ⇒ 0.4 + 0.3 + 0.1 (neutral tags) = 0.80
	d := deployAt(spike.Add(-time.Hour), "AWSLambda")
	env.seedDeploy(d)

	env.cycle()

	edges := env.edges(anomaly)
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
	top := edges[0]
	if top.Status != models.BlameStatusResolved || top.Narrative != "PR #42 did it." {
		t.Errorf("top edge status=%q narrative=%q, want resolved with the generated narrative", top.Status, top.Narrative)
	}
	if want := models.BlameEdgeID(anomaly.ID, d.ID); top.ID != want {
		t.Errorf("edge ID = %s, want the deterministic %s", top.ID, want)
	}
	if len(env.notifier.sent) != 1 {
		t.Fatalf("notifications sent = %d, want 1", len(env.notifier.sent))
	}
	g := env.notifier.sent[0]
	if g.TopBlame == nil || g.TopBlame.ID != top.ID || g.TopBlame.DeployEvent == nil || g.Anomaly.ID != anomaly.ID {
		t.Errorf("alert graph is incomplete: %+v", g)
	}
	if len(env.outbox()) != 0 {
		t.Error("delivered alert is still in the outbox")
	}
	if env.unscored() != 0 {
		t.Error("anomaly was not marked scored")
	}
}

func TestCorrelateNew_NoCandidates_StillMarksScored(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)

	env.cycle()

	if n := len(env.edges(anomaly)); n != 0 {
		t.Errorf("edges = %d, want 0", n)
	}
	if len(env.notifier.sent) != 0 {
		t.Errorf("notifications sent = %d, want 0", len(env.notifier.sent))
	}
	if env.unscored() != 0 {
		t.Fatal("anomaly with no candidates must still be marked scored")
	}
}

func TestCorrelateNew_BelowMinScore_DiscardsButMarksScored(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	// 70h old, no service match ⇒ 0.1*0.4 + 0.5*0.2 = 0.14 < minScore 0.2
	env.seedDeploy(deployAt(spike.Add(-70 * time.Hour)))

	env.cycle()

	if n := len(env.edges(anomaly)); n != 0 {
		t.Errorf("edges = %d, want 0 (below threshold)", n)
	}
	if env.unscored() != 0 {
		t.Fatal("below-threshold anomaly must still be marked scored")
	}
}

func TestCorrelateNew_ProcessingError_LeavesAnomalyUnscoredThenRecovers(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.store.deploysErr = errors.New("db locked")

	env.cycle() // must continue past the per-anomaly error

	if env.unscored() != 1 {
		t.Fatal("failed anomaly must stay unscored for retry")
	}
	if len(env.edges(anomaly)) != 0 {
		t.Fatal("no edges expected while the lookup is failing")
	}

	env.store.deploysErr = nil
	env.cycle()
	if len(env.edges(anomaly)) != 1 || env.unscored() != 0 || len(env.notifier.sent) != 1 {
		t.Errorf("retry did not recover: edges=%d unscored=%d sent=%d", len(env.edges(anomaly)), env.unscored(), len(env.notifier.sent))
	}
}

// A deploy made during the spike day is the most likely cause of that day's
// bucketed DAILY cost, so it must be a candidate and can reach the alert tier.
func TestCorrelateNew_DeployDuringPeriod_IsBlamed(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	// 10h into a 24h period + exact service match ⇒ 0.4 + 0.3 + 0.1 = 0.80
	env.seedDeploy(deployAt(spike.Add(10*time.Hour), "AWSLambda"))

	env.cycle()

	if len(env.edges(anomaly)) != 1 {
		t.Fatal("in-period deploy must be a candidate")
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("notifications sent = %d, want 1", len(env.notifier.sent))
	}
}

// A deploy after the period has ended cannot have caused its cost.
func TestCorrelateNew_DeployAfterPeriod_IsNotBlamed(t *testing.T) {
	env := newEnv(t, 0.1)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(25*time.Hour), "AWSLambda"))

	env.cycle()

	if n := len(env.edges(anomaly)); n != 0 {
		t.Fatalf("edges = %d, want 0", n)
	}
}

// Failure mode (a): the anomaly could not be marked scored after its edges were
// saved, so the next cycle processes it again. That must not create a second
// set of edges or a second alert.
func TestRetryAfterPartialFailure_DoesNotDuplicateEdgesOrAlerts(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.store.failMarkScored = 1

	env.cycle()
	if env.unscored() != 1 {
		t.Fatal("setup: the first cycle should have failed to mark the anomaly scored")
	}
	env.cycle() // reprocesses the same anomaly

	if n := len(env.edges(anomaly)); n != 1 {
		t.Errorf("edges after retry = %d, want exactly 1", n)
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("alerts sent = %d, want exactly 1", len(env.notifier.sent))
	}
	if env.unscored() != 0 {
		t.Error("anomaly should be scored after the retry")
	}
}

// Failure mode (b): a notifier outage must delay the alert, not lose it.
func TestNotifierFailure_AlertIsRetriedNotLost(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.notifier.fail = true

	env.cycle()
	if len(env.notifier.sent) != 0 || len(env.outbox()) != 1 {
		t.Fatalf("after a failed send the alert must stay queued: sent=%d outbox=%d", len(env.notifier.sent), len(env.outbox()))
	}

	env.notifier.fail = false
	env.cycle()
	if len(env.notifier.sent) != 1 {
		t.Fatalf("alert was not delivered after the notifier recovered: sent=%d", len(env.notifier.sent))
	}
	if len(env.outbox()) != 0 {
		t.Error("delivered alert is still queued")
	}

	attempts := env.notifier.attempts
	env.cycle()
	env.cycle()
	if env.notifier.attempts != attempts {
		t.Errorf("a delivered alert was re-sent (%d -> %d attempts)", attempts, env.notifier.attempts)
	}
}

// Failure mode (c): a deploy that arrives after the first scoring pass is still
// weighed, within the rescore window.
func TestLateDeploy_IsPickedUpWithinRescoreWindow(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)

	env.cycle() // no deploys yet: scored, nothing found
	if len(env.edges(anomaly)) != 0 || env.unscored() != 0 {
		t.Fatal("setup: first pass should score the anomaly with no edges")
	}

	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda")) // webhook arrives late
	env.cycle()

	if len(env.edges(anomaly)) != 1 {
		t.Fatal("late deploy was never considered")
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("alerts sent = %d, want 1", len(env.notifier.sent))
	}
}

func TestLateDeploy_IgnoredWhenRescoringIsDisabled(t *testing.T) {
	env := newEnv(t, 0.2)
	env.engine.WithRescoreWindow(0)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.cycle()

	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.cycle()

	if len(env.edges(anomaly)) != 0 {
		t.Fatal("with rescore_window 0 an anomaly must be scored exactly once")
	}
}

// Failure mode (c), enrichment variant: PR enrichment lands after the first
// pass and turns a weak candidate into a confident one.
func TestEnrichmentAfterScoring_PromotesAndAlerts(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	d := deployAt(spike.Add(-time.Hour)) // no inferred services yet ⇒ 0.4 + 0.1 = 0.50
	env.seedDeploy(d)

	env.cycle()
	edges := env.edges(anomaly)
	if len(edges) != 1 || edges[0].Status != models.BlameStatusPending || len(env.notifier.sent) != 0 {
		t.Fatalf("setup: expected one pending edge and no alert, got %d edges, %d alerts", len(edges), len(env.notifier.sent))
	}

	d.InferredServices = []string{"AWSLambda"} // enrichment arrives: ⇒ 0.80
	env.seedDeploy(d)
	env.cycle()

	edges = env.edges(anomaly)
	if len(edges) != 1 {
		t.Fatalf("enrichment must update the existing edge, not add one: got %d edges", len(edges))
	}
	if edges[0].Status != models.BlameStatusResolved || edges[0].ConfidenceScore < HighConfidenceThreshold || edges[0].Narrative == "" {
		t.Errorf("edge not promoted: status=%q score=%.2f narrative=%q", edges[0].Status, edges[0].ConfidenceScore, edges[0].Narrative)
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("alerts sent = %d, want 1", len(env.notifier.sent))
	}
}

// Re-scoring must not spam alerts: one anomaly is alerted about once, even if a
// stronger candidate shows up afterwards.
func TestOneAlertPerAnomaly(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	first := deployAt(spike.Add(-5*time.Hour), "AWSLambda") // 0.85*0.4 + 0.3 + 0.1 = 0.74
	env.seedDeploy(first)
	env.cycle()
	if len(env.notifier.sent) != 1 {
		t.Fatalf("setup: first alert missing")
	}

	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda")) // 0.80, stronger
	env.cycle()

	edges := env.edges(anomaly)
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2 candidates", len(edges))
	}
	resolved := 0
	for _, e := range edges {
		if e.Status == models.BlameStatusResolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Errorf("resolved edges = %d, want 1 (the second candidate stays pending)", resolved)
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("alerts sent = %d, want still 1", len(env.notifier.sent))
	}
}

// A human's verdict is final: re-scoring must not resurrect or re-alert it.
func TestDismissedEdgeIsNotResurrectedOrReAlerted(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.cycle()

	edge := env.edges(anomaly)[0]
	if err := env.store.UpdateBlameStatus(context.Background(), edge.ID, models.BlameStatusDismissed); err != nil {
		t.Fatal(err)
	}
	env.cycle()
	env.cycle()

	after := env.edges(anomaly)
	if len(after) != 1 || after[0].Status != models.BlameStatusDismissed {
		t.Errorf("dismissed edge was changed: %+v", after)
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("alerts sent = %d, want 1 (no re-alert after dismissal)", len(env.notifier.sent))
	}
}

// Re-scoring with nothing new must be free: no writes, no new rows.
func TestUnchangedRescoreWritesNothing(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.cycle()
	savesAfterFirst := env.store.saves
	before := env.edges(anomaly)

	for i := 0; i < 5; i++ {
		env.cycle()
	}

	if env.store.saves != savesAfterFirst {
		t.Errorf("SaveBlameEdges called %d extra time(s) while nothing changed", env.store.saves-savesAfterFirst)
	}
	after := env.edges(anomaly)
	if len(after) != 1 || after[0].ID != before[0].ID || !after[0].CreatedAt.Equal(before[0].CreatedAt) {
		t.Errorf("edge changed across no-op re-scores: %+v -> %+v", before, after)
	}
}

// Alerts that could not be delivered for longer than maxAlertAge are dropped
// rather than sent hours or days late.
func TestStaleUndeliveredAlertsAreNotSent(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.notifier.fail = true
	env.cycle()
	if len(env.outbox()) != 1 {
		t.Fatal("setup: expected a queued alert")
	}

	env.notifier.fail = false
	env.engine.now = func() time.Time { return time.Now().Add(maxAlertAge + 24*time.Hour) }
	env.cycle()

	if len(env.notifier.sent) != 0 {
		t.Errorf("a stale alert was delivered (%d sent)", len(env.notifier.sent))
	}
}

// Re-scoring every cycle would re-run the candidate queries for every recent
// anomaly each minute; passes are spaced out by rescoreGap instead.
func TestRescoreGap_LimitsHowOftenAnomaliesAreRescored(t *testing.T) {
	env := newEnv(t, 0.2)
	env.engine.rescoreGap = DefaultRescoreGap
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.cycle() // scores the anomaly; no deploys yet

	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda")) // arrives late
	env.cycle()                                                  // too soon after the last re-scoring pass
	if len(env.edges(anomaly)) != 0 {
		t.Fatal("re-scored again inside the gap")
	}

	env.engine.now = func() time.Time { return time.Now().Add(DefaultRescoreGap + time.Minute) }
	env.cycle()
	if len(env.edges(anomaly)) != 1 {
		t.Fatal("late deploy was not picked up once the gap had passed")
	}
}

// A promoted edge keeps its original created_at, so the alert cutoff must grow
// with the rescore window or long windows would silently drop alerts.
func TestAlertCutoff_GrowsWithTheRescoreWindow(t *testing.T) {
	env := newEnv(t, 0.2)
	now := time.Now()
	env.engine.now = func() time.Time { return now }

	for window, wantAge := range map[time.Duration]time.Duration{
		0:              maxAlertAge,
		24 * time.Hour: maxAlertAge, // 2x24h = 48h, equal to the floor
		72 * time.Hour: 144 * time.Hour,
	} {
		env.engine.WithRescoreWindow(window)
		if got := now.Sub(env.engine.alertCutoff()); got != wantAge {
			t.Errorf("rescore window %v: alerts retried for %v, want %v", window, got, wantAge)
		}
	}
}

func TestLongRescoreWindow_StillDeliversAnAlertQueuedForOverTwoDays(t *testing.T) {
	env := newEnv(t, 0.2)
	env.engine.WithRescoreWindow(72 * time.Hour)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	env.seedDeploy(deployAt(spike.Add(-time.Hour), "AWSLambda"))
	env.notifier.fail = true
	env.cycle()
	if len(env.outbox()) != 1 {
		t.Fatal("setup: expected a queued alert")
	}

	env.notifier.fail = false
	env.engine.now = func() time.Time { return time.Now().Add(60 * time.Hour) } // past the fixed 48h
	env.cycle()

	if len(env.notifier.sent) != 1 {
		t.Errorf("alert queued for 60h was dropped despite a 72h rescore window (sent=%d)", len(env.notifier.sent))
	}
}

// Re-scoring many anomalies must not issue one history query per candidate.
func TestHistoryCountsAreCachedWithinACycle(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	env.seedAnomaly(anomaly)
	for _, h := range []time.Duration{1, 2, 3, 4} { // four candidates, same author and service
		env.seedDeploy(deployAt(spike.Add(-h*time.Hour), "AWSLambda"))
	}

	env.cycle()
	if got := env.store.historyQueries; got != 1 {
		t.Errorf("history queries in one cycle = %d, want 1 for the shared (author, service)", got)
	}

	before := env.store.historyQueries
	env.cycle() // a new cycle starts with a fresh cache, then re-scores the anomaly
	if got := env.store.historyQueries - before; got != 1 {
		t.Errorf("history queries in the next cycle = %d, want 1", got)
	}
}
