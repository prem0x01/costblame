// Package correlate is the core of costblame: it picks up unblamed cost anomalies,
// scores every deployment candidate in the lookback window, persists the resulting
// blame edges, and triggers narrative generation + alerting for high-confidence hits.
package correlate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/prem0x01/costblame/pkg/models"
)

// CandidateWindow is the maximum time before a cost spike to consider a deploy causal.
const CandidateWindow = 72 * time.Hour

const (
	// DefaultRescoreWindow is how long after first scoring an anomaly keeps being
	// re-scored, so deploys and PR enrichment that arrive late are still weighed.
	DefaultRescoreWindow = 24 * time.Hour

	// DefaultRescoreGap is the minimum time between re-scoring passes. Re-scoring
	// every cycle would re-run the candidate queries for every recent anomaly
	// each minute on the one SQLite connection the webhooks and UI also use; a
	// late deploy or enrichment is picked up within this gap instead.
	DefaultRescoreGap = 5 * time.Minute

	// maxAlertAge is the minimum time an undelivered alert keeps being retried
	// before it is dropped from the outbox as stale. The effective limit is
	// larger when the rescore window is (see alertCutoff).
	maxAlertAge = 48 * time.Hour
)

// EngineStore is the subset of store.Store that the Engine requires.
// Using a narrow interface keeps the engine testable without a real database.
type EngineStore interface {
	UnscoredAnomalies(ctx context.Context) ([]models.CostSnapshot, error)
	RecentlyScoredAnomalies(ctx context.Context, since time.Time) ([]models.CostSnapshot, error)
	MarkAnomalyScored(ctx context.Context, id uuid.UUID) error
	DeploysBetween(ctx context.Context, from, to time.Time) ([]models.DeployEvent, error)
	BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error)
	SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error
	UnalertedEdges(ctx context.Context, since time.Time) ([]models.BlameEdge, error)
	MarkEdgeAlerted(ctx context.Context, id uuid.UUID) error
	ConfirmedBlameCount(ctx context.Context, author, service string) (int, error)
}

// NarrativeGenerator produces a human-readable blame narrative for a resolved edge.
type NarrativeGenerator interface {
	Generate(ctx context.Context, edge models.BlameEdge) (string, error)
}

// Notifier sends blame alerts to configured channels (Slack, webhook, etc.).
type Notifier interface {
	Send(ctx context.Context, graph models.BlameGraph) error
}

// Engine runs the correlation loop, pairing cost anomalies with deploy events.
//
// Each cycle it (1) re-scores anomalies first scored within the rescore window,
// (2) scores anomalies not yet scored, and (3) delivers pending alerts. All three
// are idempotent: edges are keyed by their (anomaly, deploy) pair, and an alert
// is only marked delivered after a notifier accepts it, so a crash or a failing
// notifier at any point is repaired by the next cycle instead of duplicating
// edges or losing the alert. Delivery is therefore at-least-once.
type Engine struct {
	store         EngineStore
	scorer        *Scorer
	narrative     NarrativeGenerator
	notifier      Notifier
	interval      time.Duration
	minScore      float64
	rescoreWindow time.Duration
	rescoreGap    time.Duration
	lastRescore   time.Time
	history       *storeHistoryAdapter
	now           func() time.Time
}

// NewEngine creates an Engine. interval controls how often the loop runs;
// minScore is the lower bound below which edges are discarded (noise filter).
func NewEngine(
	store EngineStore,
	narrative NarrativeGenerator,
	notifier Notifier,
	interval time.Duration,
	minScore float64,
) *Engine {
	if interval <= 0 {
		interval = time.Minute
	}
	history := &storeHistoryAdapter{store: store}
	return &Engine{
		store:         store,
		scorer:        NewScorer(history),
		history:       history,
		narrative:     narrative,
		notifier:      notifier,
		interval:      interval,
		minScore:      minScore,
		rescoreWindow: DefaultRescoreWindow,
		rescoreGap:    DefaultRescoreGap,
		now:           time.Now,
	}
}

// WithRescoreWindow sets how long an anomaly keeps being re-scored after it was
// first scored. Zero disables re-scoring.
func (e *Engine) WithRescoreWindow(d time.Duration) *Engine {
	e.rescoreWindow = d
	return e
}

// Run starts the correlation loop and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	// Run once immediately so the first cycle doesn't wait for the full interval.
	if err := e.correlateNew(ctx); err != nil {
		slog.Error("initial correlation cycle failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.correlateNew(ctx); err != nil {
				slog.Error("correlation cycle failed", "err", err)
			}
		}
	}
}

// correlateNew runs one cycle: re-score recent anomalies, score new ones, then
// deliver any pending alerts.
func (e *Engine) correlateNew(ctx context.Context) error {
	e.history.reset() // history counts are cached for one cycle only

	// Re-score before marking this cycle's new anomalies scored, so they are not
	// processed twice in one cycle.
	e.rescoreRecent(ctx)

	anomalies, err := e.store.UnscoredAnomalies(ctx)
	if err != nil {
		e.deliverAlerts(ctx) // outstanding alerts must not wait on an unrelated failure
		return fmt.Errorf("fetching unscored anomalies: %w", err)
	}

	if len(anomalies) > 0 {
		slog.Info("correlation cycle", "unscored_anomalies", len(anomalies))
	}
	for _, anomaly := range anomalies {
		if err := e.processAnomaly(ctx, anomaly); err != nil {
			slog.Error("processing anomaly", "anomaly_id", anomaly.ID, "err", err)
			// Continue processing other anomalies — one bad record shouldn't halt
			// everything. The anomaly stays unscored and is retried next cycle;
			// saves are idempotent, so the retry cannot duplicate edges.
			continue
		}
		// Mark scored even when no edges resulted, so anomalies with no
		// qualifying deploys aren't reprocessed as new on every cycle forever.
		if err := e.store.MarkAnomalyScored(ctx, anomaly.ID); err != nil {
			slog.Error("marking anomaly scored", "anomaly_id", anomaly.ID, "err", err)
		}
	}

	e.deliverAlerts(ctx)
	return nil
}

// rescoreRecent re-evaluates anomalies first scored within the rescore window.
// Edges are idempotent, so this only changes anything when something new has
// happened: a late deploy event, or enrichment that improved a candidate's
// service match.
func (e *Engine) rescoreRecent(ctx context.Context) {
	if e.rescoreWindow <= 0 {
		return
	}
	now := e.now()
	if !e.lastRescore.IsZero() && now.Sub(e.lastRescore) < e.rescoreGap {
		return
	}
	e.lastRescore = now
	recent, err := e.store.RecentlyScoredAnomalies(ctx, now.Add(-e.rescoreWindow))
	if err != nil {
		slog.Error("fetching recently scored anomalies", "err", err)
		return
	}
	for _, anomaly := range recent {
		if err := e.processAnomaly(ctx, anomaly); err != nil {
			slog.Error("re-scoring anomaly", "anomaly_id", anomaly.ID, "err", err)
		}
	}
}

// processAnomaly scores every deploy candidate for one anomaly and saves the
// edges that are new or changed, promoting the top candidate to "resolved"
// (narrative included) when it is confident enough and the anomaly has not been
// alerted already. It does not send alerts; deliverAlerts does, from the outbox.
func (e *Engine) processAnomaly(ctx context.Context, anomaly models.CostSnapshot) error {
	// The window runs through PeriodEnd, not PeriodStart: a billing period is a
	// bucket (a full UTC day for DAILY data), so deploys made during the period
	// are the most likely cause of its spike.
	windowStart := anomaly.PeriodStart.Add(-CandidateWindow)
	windowEnd := anomaly.PeriodEnd
	if windowEnd.Before(anomaly.PeriodStart) {
		windowEnd = anomaly.PeriodStart
	}
	candidates, err := e.store.DeploysBetween(ctx, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("fetching candidate deploys: %w", err)
	}
	if len(candidates) == 0 {
		slog.Debug("no deploy candidates", "anomaly_id", anomaly.ID, "service", anomaly.Service)
		return nil
	}

	existing, err := e.store.BlameEdgesBySnapshot(ctx, anomaly.ID)
	if err != nil {
		return fmt.Errorf("fetching existing edges: %w", err)
	}
	byDeploy := make(map[uuid.UUID]models.BlameEdge, len(existing))
	alerted := false // an alert already went out (or is queued) for this anomaly
	for _, ex := range existing {
		byDeploy[ex.DeployEventID] = ex
		if ex.Status == models.BlameStatusResolved || ex.Status == models.BlameStatusConfirmed {
			alerted = true
		}
	}

	// Score each candidate and discard below-threshold edges.
	edges := make([]models.BlameEdge, 0, len(candidates))
	for _, deploy := range candidates {
		factors := e.scorer.Score(ctx, anomaly, deploy)
		score := e.scorer.TotalScore(factors)
		if score < e.minScore {
			continue
		}

		d := deploy
		snap := anomaly
		edges = append(edges, models.BlameEdge{
			// Deterministic: the same (anomaly, deploy) pair is always the same edge.
			ID:                models.BlameEdgeID(anomaly.ID, deploy.ID),
			CostSnapshotID:    anomaly.ID,
			DeployEventID:     deploy.ID,
			ConfidenceScore:   score,
			ConfidenceFactors: factors,
			Status:            models.BlameStatusPending,
			CreatedAt:         e.now().UTC(),
			CostSnapshot:      &snap,
			DeployEvent:       &d,
		})
	}
	if len(edges) == 0 {
		slog.Debug("no high-enough candidates", "anomaly_id", anomaly.ID)
		return nil
	}

	// Sort descending so edges[0] is the highest-confidence blame.
	sort.Slice(edges, func(i, j int) bool {
		return edges[i].ConfidenceScore > edges[j].ConfidenceScore
	})

	// Promote the best alertable edge (confident enough AND with evidence that
	// the deploy touches the spiking service) if nobody has been alerted about
	// this anomaly yet and a human has not already ruled on that very edge. The
	// narrative is generated before the save so the edge is written once, in its
	// final state.
	if !alerted {
		for i := range edges {
			if !alertable(edges[i]) {
				continue
			}
			cand := &edges[i]
			if ex, seen := byDeploy[cand.DeployEventID]; !seen || ex.Status == models.BlameStatusPending {
				if narrative, err := e.narrative.Generate(ctx, *cand); err != nil {
					slog.Warn("narrative generation failed", "edge_id", cand.ID, "err", err)
				} else {
					cand.Narrative = narrative
				}
				cand.Status = models.BlameStatusResolved
			}
			break // one alert per anomaly: the best candidate, whatever its state
		}
	}

	// Save only what is new or changed; an unchanged re-score writes nothing.
	var changed []models.BlameEdge
	for _, edge := range edges {
		ex, seen := byDeploy[edge.DeployEventID]
		switch {
		case !seen:
			changed = append(changed, edge)
		case ex.Status != models.BlameStatusPending:
			// Reviewed or already alerted: the store would leave it alone anyway.
		case ex.ConfidenceScore != edge.ConfidenceScore || ex.Status != edge.Status:
			changed = append(changed, edge)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := e.store.SaveBlameEdges(ctx, changed); err != nil {
		return fmt.Errorf("saving blame edges: %w", err)
	}

	slog.Info("blame edges saved",
		"anomaly_id", anomaly.ID,
		"service", anomaly.Service,
		"edges", len(changed),
		"top_score", fmt.Sprintf("%.2f", edges[0].ConfidenceScore),
	)
	return nil
}

// alertable reports whether an edge may become an alert. Score alone is not
// enough: the edge must also show evidence that the deploy touches the spiking
// service (a non-zero service_match). Tags, timing and author history can add up
// to the threshold without it (a prod deploy by a repeat offender from the right
// team, in the right hour), but then nothing links the deploy to the service that
// spiked, and an alert would be a guess presented as an answer.
func alertable(e models.BlameEdge) bool {
	if e.ConfidenceScore < HighConfidenceThreshold {
		return false
	}
	for _, f := range e.ConfidenceFactors {
		if f.Name == "service_match" {
			return f.Score > 0
		}
	}
	return false
}

// alertCutoff is the oldest edge creation time whose alert is still worth
// delivering. An edge is created when first saved as pending, but may only be
// promoted to resolved (and so become alertable) up to rescoreWindow later, so
// the limit must grow with the window: with a fixed cutoff a long rescore window
// would let promoted edges be silently dropped.
func (e *Engine) alertCutoff() time.Time {
	age := maxAlertAge
	if w := 2 * e.rescoreWindow; w > age {
		age = w
	}
	return e.now().Add(-age)
}

// deliverAlerts sends every resolved-but-undelivered alert and marks each one
// delivered only after a notifier accepted it. A failure is logged and retried
// on the next cycle (until maxAlertAge), so an outage delays alerts rather than
// losing them.
func (e *Engine) deliverAlerts(ctx context.Context) {
	pending, err := e.store.UnalertedEdges(ctx, e.alertCutoff())
	if err != nil {
		slog.Error("fetching undelivered alerts", "err", err)
		return
	}
	for _, edge := range pending {
		if edge.CostSnapshot == nil {
			continue
		}
		top := edge
		edges, err := e.store.BlameEdgesBySnapshot(ctx, edge.CostSnapshotID)
		if err != nil || len(edges) == 0 {
			edges = []models.BlameEdge{edge}
		}
		graph := models.BlameGraph{
			ID:          uuid.New(),
			Anomaly:     *edge.CostSnapshot,
			Edges:       edges,
			TopBlame:    &top,
			GeneratedAt: e.now().UTC(),
		}
		if err := e.notifier.Send(ctx, graph); err != nil {
			slog.Warn("notification failed, will retry next cycle", "edge_id", edge.ID, "err", err)
			continue
		}
		if err := e.store.MarkEdgeAlerted(ctx, edge.ID); err != nil {
			slog.Error("alert delivered but could not be recorded; it may be sent again", "edge_id", edge.ID, "err", err)
		}
	}
}

// storeHistoryAdapter adapts EngineStore to HistoryReader for the Scorer.
// It synchronously queries the DB; historical scoring is cheap (one COUNT query).
//
// Counts are cached until reset() so that re-scoring many anomalies in one
// cycle asks the database once per (author, service), not once per candidate.
// The engine calls it from a single goroutine, so no locking is needed.
type storeHistoryAdapter struct {
	store EngineStore
	cache map[string]int
}

// reset drops cached counts; the engine calls it at the start of each cycle.
func (a *storeHistoryAdapter) reset() { a.cache = nil }

func (a *storeHistoryAdapter) ConfirmedBlameCount(ctx context.Context, author, service string) int {
	key := author + "\x00" + service
	if n, ok := a.cache[key]; ok {
		return n
	}
	count, err := a.store.ConfirmedBlameCount(ctx, author, service)
	if err != nil {
		slog.Warn("confirmed blame count lookup failed — historical factor scores 0",
			"author", author, "service", service, "err", err)
		return 0 // not cached: a transient failure should be retried
	}
	if a.cache == nil {
		a.cache = make(map[string]int)
	}
	a.cache[key] = count
	return count
}
