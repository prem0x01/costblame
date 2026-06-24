package correlate

import (
	"fmt"
	"math"
	"strings"

	"github.com/prem0x01/costblame/pkg/models"
)

const (
	weightTemporal   = 0.40
	weightService    = 0.30
	weightTag        = 0.20
	weightHistorical = 0.10

	// HighConfidenceThreshold triggers narrative generation and Slack alerting.
	HighConfidenceThreshold = 0.65
)

// Scorer computes a confidence score for a (cost anomaly, deploy event) pair
// using four independent factors. Each factor returns a score in [0, 1] and
// has a fixed weight; the final score is their weighted sum, capped at 1.0.
type Scorer struct {
	history HistoryReader
}

// HistoryReader provides historical blame data used by the historicalScore factor.
type HistoryReader interface {
	ConfirmedBlameCount(author, service string) int
}

// NewScorer creates a Scorer backed by the given history reader.
func NewScorer(h HistoryReader) *Scorer {
	return &Scorer{history: h}
}

// Score returns the individual ConfidenceFactors for a (anomaly, deploy) pair.
func (s *Scorer) Score(anomaly models.CostSnapshot, deploy models.DeployEvent) []models.ConfidenceFactor {
	return []models.ConfidenceFactor{
		s.temporalScore(anomaly, deploy),
		s.serviceScore(anomaly, deploy),
		s.tagScore(anomaly, deploy),
		s.historicalScore(anomaly, deploy),
	}
}

// TotalScore sums all factor contributions into a single [0, 1] confidence score.
func (s *Scorer) TotalScore(factors []models.ConfidenceFactor) float64 {
	var total float64
	for _, f := range factors {
		total += f.Score * f.Weight
	}
	return math.Min(total, 1.0)
}

// temporalScore rewards deploys that happened just before the cost spike.
// The window is 72 hours; anything older scores 0 to avoid false positives
// from unrelated old deployments.
func (s *Scorer) temporalScore(anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	hoursApart := anomaly.PeriodStart.Sub(deploy.OccurredAt).Hours()

	var score float64
	var reason string

	switch {
	case hoursApart < 0:
		// Deploy happened after the cost period ended — cannot be the cause.
		score = 0
		reason = "deploy occurred after cost period"
	case hoursApart <= 2:
		score = 1.0
		reason = fmt.Sprintf("deploy %.1fh before spike (≤2h window)", hoursApart)
	case hoursApart <= 6:
		score = 0.85
		reason = fmt.Sprintf("deploy %.1fh before spike (2–6h window)", hoursApart)
	case hoursApart <= 24:
		score = 0.60
		reason = fmt.Sprintf("deploy %.1fh before spike (6–24h window)", hoursApart)
	case hoursApart <= 48:
		score = 0.30
		reason = fmt.Sprintf("deploy %.1fh before spike (24–48h window)", hoursApart)
	case hoursApart <= 72:
		score = 0.10
		reason = fmt.Sprintf("deploy %.1fh before spike (48–72h window)", hoursApart)
	default:
		score = 0
		reason = fmt.Sprintf("deploy too old (%.0fh > 72h)", hoursApart)
	}

	return models.ConfidenceFactor{
		Name: "temporal_proximity", Score: score, Weight: weightTemporal, Reason: reason,
	}
}

// serviceScore measures how well the deploy's inferred services match the anomaly service.
func (s *Scorer) serviceScore(anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	var score float64
	var reason string

	for _, svc := range deploy.InferredServices {
		if strings.EqualFold(svc, anomaly.Service) {
			score = 1.0
			reason = "exact service match: " + svc
			break
		}
		// Fuzzy: "ecs" in a path matches "AmazonECS" service name.
		if serviceContains(anomaly.Service, svc) && score < 0.70 {
			score = 0.70
			reason = "partial service match: " + svc + " ~ " + anomaly.Service
		}
	}

	if score == 0 {
		reason = "no service match found in changed files"
	}

	return models.ConfidenceFactor{
		Name: "service_match", Score: score, Weight: weightService, Reason: reason,
	}
}

// tagScore compares the anomaly's resource tags against the deploy's team/env metadata.
// When the anomaly has no tags, the factor returns a neutral 0.5 — unknown, not negative.
func (s *Scorer) tagScore(anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	if len(anomaly.Tags) == 0 {
		return models.ConfidenceFactor{
			Name: "tag_match", Score: 0.5, Weight: weightTag,
			Reason: "no tags on anomaly — neutral score applied",
		}
	}

	matches, checks := 0, 0

	if team := anomaly.Tags["team"]; team != "" {
		checks++
		if strings.EqualFold(team, deploy.PRTeam) || strings.EqualFold(team, repoToTeam(deploy.Repository)) {
			matches++
		}
	}

	if env := anomaly.Tags["env"]; env != "" {
		checks++
		if strings.EqualFold(env, deploy.Environment) {
			matches++
		}
	}

	var score float64
	if checks > 0 {
		score = float64(matches) / float64(checks)
	}

	return models.ConfidenceFactor{
		Name:   "tag_match",
		Score:  score,
		Weight: weightTag,
		Reason: fmt.Sprintf("%d/%d resource tags matched", matches, checks),
	}
}

// historicalScore boosts the score when the same author has caused confirmed cost
// spikes on the same service before — up to a cap of 4 prior incidents.
func (s *Scorer) historicalScore(anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	count := s.history.ConfirmedBlameCount(deploy.PRAuthor, anomaly.Service)
	score := math.Min(float64(count)*0.25, 1.0)
	return models.ConfidenceFactor{
		Name:   "historical_pattern",
		Score:  score,
		Weight: weightHistorical,
		Reason: fmt.Sprintf("%d prior confirmed blame(s) for %s on %s", count, deploy.PRAuthor, anomaly.Service),
	}
}

// repoToTeam extracts a simple team slug from "org/team-service" style repo names.
// e.g. "acme/payments-api" → "payments".
func repoToTeam(repo string) string {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) < 2 {
		return repo
	}
	name := parts[1]
	// Strip common suffixes: -api, -service, -worker, -backend
	for _, suffix := range []string{"-api", "-service", "-worker", "-backend", "-server"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}
