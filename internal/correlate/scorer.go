package correlate

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

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
	ConfirmedBlameCount(ctx context.Context, author, service string) int
}

// NewScorer creates a Scorer backed by the given history reader.
func NewScorer(h HistoryReader) *Scorer {
	return &Scorer{history: h}
}

// Score returns the individual ConfidenceFactors for a (anomaly, deploy) pair.
//
// A factor that cannot be evaluated (the tag factor when the anomaly carries no
// usable allocation tags) is skipped: its weight is 0 and the remaining weights
// are rescaled to sum to 1. Each factor's Weight is therefore its actual share
// of the total, the total can span the full 0..1 range, and "unknown" no longer
// adds a fixed 0.10 to every score the way a neutral 0.5 did.
func (s *Scorer) Score(ctx context.Context, anomaly models.CostSnapshot, deploy models.DeployEvent) []models.ConfidenceFactor {
	return normalizeWeights([]models.ConfidenceFactor{
		s.temporalScore(anomaly, deploy),
		s.serviceScore(anomaly, deploy),
		s.tagScore(anomaly, deploy),
		s.historicalScore(ctx, anomaly, deploy),
	})
}

// normalizeWeights rescales factor weights so they sum to 1. When every factor
// is applied they already do and nothing changes.
func normalizeWeights(factors []models.ConfidenceFactor) []models.ConfidenceFactor {
	var sum float64
	for _, f := range factors {
		sum += f.Weight
	}
	if sum <= 0 || math.Abs(sum-1) < 1e-9 {
		return factors
	}
	for i := range factors {
		factors[i].Weight /= sum
	}
	return factors
}

// TotalScore sums all factor contributions into a single [0, 1] confidence score.
func (s *Scorer) TotalScore(factors []models.ConfidenceFactor) float64 {
	var total float64
	for _, f := range factors {
		total += f.Score * f.Weight
	}
	return math.Min(total, 1.0)
}

// lateInPeriodFraction is the share of the cost period that must still remain
// after a deploy for it to get full credit. A deploy in the last quarter of a
// period had little time to accrue cost, so it is scored down, not excluded.
const lateInPeriodFraction = 0.25

// temporalScore rewards deploys that happened just before or during the cost
// period. Billing periods are buckets (a whole UTC day for DAILY data), so a
// deploy at 10:00 can be the cause of that day's spike: deploys inside
// [PeriodStart, PeriodEnd] are scored by how much of the period they had to
// act, and earlier deploys decay over 72 hours. Anything older scores 0 to
// avoid false positives from unrelated old deployments.
func (s *Scorer) temporalScore(anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	// A snapshot without a usable PeriodEnd is treated as a point in time.
	periodEnd := anomaly.PeriodEnd
	if periodEnd.Before(anomaly.PeriodStart) {
		periodEnd = anomaly.PeriodStart
	}

	if deploy.OccurredAt.After(periodEnd) {
		// Deploy happened after the cost period ended — cannot be the cause.
		return models.ConfidenceFactor{
			Name: "temporal_proximity", Score: 0, Weight: weightTemporal,
			Reason: "deploy occurred after cost period",
		}
	}

	if deploy.OccurredAt.After(anomaly.PeriodStart) {
		period := periodEnd.Sub(anomaly.PeriodStart)
		remaining := periodEnd.Sub(deploy.OccurredAt)
		score := 1.0
		reason := fmt.Sprintf("deploy %.1fh into the cost period, %.1fh before it ended", deploy.OccurredAt.Sub(anomaly.PeriodStart).Hours(), remaining.Hours())
		if remaining < time.Duration(float64(period)*lateInPeriodFraction) {
			score = 0.5
			reason += " (late in period — limited exposure)"
		}
		return models.ConfidenceFactor{
			Name: "temporal_proximity", Score: score, Weight: weightTemporal, Reason: reason,
		}
	}

	hoursApart := anomaly.PeriodStart.Sub(deploy.OccurredAt).Hours()

	var score float64
	var reason string

	switch {
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

	// Compare canonical identities, not raw strings: Cost Explorer reports
	// "Amazon Elastic Compute Cloud - Compute" where the service map says
	// "AmazonEC2" (see CanonicalService).
	target := CanonicalService(anomaly.Service)
	for _, svc := range deploy.InferredServices {
		c := CanonicalService(svc)
		if c == "" || target == "" {
			continue
		}
		if c == target {
			score = 1.0
			reason = "exact service match: " + svc + " = " + anomaly.Service
			break
		}
		// Partial: the anomaly's service name contains the inferred one
		// ("Amazon Kinesis Firehose" ~ Kinesis). Names that merely start with
		// another service's name are aliased to their own identity instead (see
		// serviceAliases), so they cannot borrow its credit.
		if len(c) >= minPartialLen && strings.Contains(target, c) && score < 0.70 {
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

// tagScore compares the anomaly's team and env tags with the deploy (see
// tagEvidence). It is skipped (weight 0) when there is nothing to compare: no
// tags, tags that are not team/env, or a deploy whose environment and team
// cannot be told. Unknown is not evidence for or against, and Score rescales the
// other weights.
func (s *Scorer) tagScore(anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	skipped := func(why string) models.ConfidenceFactor {
		return models.ConfidenceFactor{
			Name: "tag_match", Score: 0, Weight: 0,
			Reason: why + " — factor not applied, other weights rescaled",
		}
	}
	if len(anomaly.Tags) == 0 {
		return skipped("no team/env allocation tags on the anomaly")
	}
	if anomaly.Tags["team"] == "" && anomaly.Tags["env"] == "" {
		return skipped("the anomaly's tags include no team or env tag")
	}

	matches, checks, reason := tagEvidence(anomaly.Tags, deploy)
	if checks == 0 {
		return skipped("the deploy's environment and team cannot be compared with the anomaly's tags")
	}
	return models.ConfidenceFactor{
		Name:   "tag_match",
		Score:  float64(matches) / float64(checks),
		Weight: weightTag,
		Reason: fmt.Sprintf("%d/%d tags matched (%s)", matches, checks, reason),
	}
}

// historicalScore boosts the score when the same author has caused confirmed cost
// spikes on the same service before — up to a cap of 4 prior incidents.
func (s *Scorer) historicalScore(ctx context.Context, anomaly models.CostSnapshot, deploy models.DeployEvent) models.ConfidenceFactor {
	count := s.history.ConfirmedBlameCount(ctx, deploy.PRAuthor, anomaly.Service)
	score := math.Min(float64(count)*0.25, 1.0)
	return models.ConfidenceFactor{
		Name:   "historical_pattern",
		Score:  score,
		Weight: weightHistorical,
		Reason: fmt.Sprintf("%d prior confirmed blame(s) for %s on %s", count, deploy.PRAuthor, anomaly.Service),
	}
}
