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

// EngineStore is the subset of store.Store that the Engine requires.
// Using a narrow interface keeps the engine testable without a real database.
type EngineStore interface {
	UnblamedAnomalies(ctx context.Context) ([]models.CostSnapshot, error)
	DeploysBetween(ctx context.Context, from, to time.Time) ([]models.DeployEvent, error)
	SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error
	UpdateBlameEdge(ctx context.Context, edge models.BlameEdge) error
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
type Engine struct {
	store     EngineStore
	scorer    *Scorer
	narrative NarrativeGenerator
	notifier  Notifier
	interval  time.Duration
	minScore  float64
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
	return &Engine{
		store:     store,
		scorer:    NewScorer(&storeHistoryAdapter{store: store}),
		narrative: narrative,
		notifier:  notifier,
		interval:  interval,
		minScore:  minScore,
	}
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

// correlateNew processes all anomalies that don't yet have blame edges.
func (e *Engine) correlateNew(ctx context.Context) error {
	anomalies, err := e.store.UnblamedAnomalies(ctx)
	if err != nil {
		return fmt.Errorf("fetching unblamed anomalies: %w", err)
	}

	if len(anomalies) == 0 {
		return nil
	}

	slog.Info("correlation cycle", "unblamed_anomalies", len(anomalies))

	for _, anomaly := range anomalies {
		if err := e.processAnomaly(ctx, anomaly); err != nil {
			slog.Error("processing anomaly", "anomaly_id", anomaly.ID, "err", err)
			// Continue processing other anomalies — one bad record shouldn't halt everything.
		}
	}
	return nil
}

func (e *Engine) processAnomaly(ctx context.Context, anomaly models.CostSnapshot) error {
	windowStart := anomaly.PeriodStart.Add(-CandidateWindow)
	candidates, err := e.store.DeploysBetween(ctx, windowStart, anomaly.PeriodStart)
	if err != nil {
		return fmt.Errorf("fetching candidate deploys: %w", err)
	}

	if len(candidates) == 0 {
		slog.Debug("no deploy candidates", "anomaly_id", anomaly.ID, "service", anomaly.Service)
		return nil
	}

	// Score each candidate and discard below-threshold edges.
	edges := make([]models.BlameEdge, 0, len(candidates))
	for _, deploy := range candidates {
		factors := e.scorer.Score(anomaly, deploy)
		score := e.scorer.TotalScore(factors)

		if score < e.minScore {
			continue
		}

		// Capture loop variable for pointer safety.
		d := deploy
		snap := anomaly
		edges = append(edges, models.BlameEdge{
			ID:                uuid.New(),
			CostSnapshotID:    anomaly.ID,
			DeployEventID:     deploy.ID,
			ConfidenceScore:   score,
			ConfidenceFactors: factors,
			Status:            models.BlameStatusPending,
			CreatedAt:         time.Now().UTC(),
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

	if err := e.store.SaveBlameEdges(ctx, edges); err != nil {
		return fmt.Errorf("saving blame edges: %w", err)
	}

	slog.Info("blame edges saved",
		"anomaly_id", anomaly.ID,
		"service", anomaly.Service,
		"edges", len(edges),
		"top_score", fmt.Sprintf("%.2f", edges[0].ConfidenceScore),
	)

	// Generate narrative and alert only for high-confidence top edge.
	top := &edges[0]
	if top.ConfidenceScore < HighConfidenceThreshold {
		return nil
	}

	narrative, err := e.narrative.Generate(ctx, *top)
	if err != nil {
		slog.Warn("narrative generation failed", "edge_id", top.ID, "err", err)
	} else {
		top.Narrative = narrative
	}

	top.Status = models.BlameStatusResolved
	if err := e.store.UpdateBlameEdge(ctx, *top); err != nil {
		return fmt.Errorf("updating blame edge: %w", err)
	}

	graph := models.BlameGraph{
		ID:          uuid.New(),
		Anomaly:     anomaly,
		Edges:       edges,
		TopBlame:    top,
		GeneratedAt: time.Now().UTC(),
	}

	if err := e.notifier.Send(ctx, graph); err != nil {
		slog.Warn("notification failed", "graph_id", graph.ID, "err", err)
	}

	return nil
}

// storeHistoryAdapter adapts EngineStore to HistoryReader for the Scorer.
// It synchronously queries the DB; historical scoring is cheap (one COUNT query).
type storeHistoryAdapter struct {
	store EngineStore
}

func (a *storeHistoryAdapter) ConfirmedBlameCount(author, service string) int {
	count, err := a.store.ConfirmedBlameCount(context.Background(), author, service)
	if err != nil {
		return 0
	}
	return count
}
