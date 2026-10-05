package correlate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

type fakeHistory struct{ count int }

func (f *fakeHistory) ConfirmedBlameCount(_ context.Context, _, _ string) int { return f.count }

func TestTemporalScore(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	now := time.Now()

	cases := []struct {
		delta   time.Duration
		wantMin float64
		wantMax float64
	}{
		{30 * time.Minute, 0.99, 1.0},
		{3 * time.Hour, 0.84, 0.86},
		{12 * time.Hour, 0.59, 0.61},
		{36 * time.Hour, 0.29, 0.31},
		{60 * time.Hour, 0.09, 0.11},
		{80 * time.Hour, 0.0, 0.0},
	}

	snap := models.CostSnapshot{Service: "AmazonEC2", PeriodStart: now}
	for _, tc := range cases {
		deploy := models.DeployEvent{OccurredAt: now.Add(-tc.delta)}
		factors := s.Score(context.Background(), snap, deploy)

		var temporal float64
		for _, f := range factors {
			if f.Name == "temporal_proximity" {
				temporal = f.Score
				break
			}
		}
		if temporal < tc.wantMin || temporal > tc.wantMax {
			t.Errorf("delta=%v: temporal=%.2f want [%.2f, %.2f]",
				tc.delta, temporal, tc.wantMin, tc.wantMax)
		}
	}
}

func TestServiceMatchScore(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	now := time.Now()
	deploy := models.DeployEvent{
		OccurredAt:       now.Add(-1 * time.Hour),
		InferredServices: []string{"AmazonEC2", "AmazonS3"},
	}

	matchSnap := models.CostSnapshot{Service: "AmazonEC2"}
	noMatchSnap := models.CostSnapshot{Service: "AmazonRDS"}

	factorsMatch := s.Score(context.Background(), matchSnap, deploy)
	factorsNoMatch := s.Score(context.Background(), noMatchSnap, deploy)

	scoreOf := func(factors []models.ConfidenceFactor, name string) float64 {
		for _, f := range factors {
			if f.Name == name {
				return f.Score
			}
		}
		return -1
	}

	if scoreOf(factorsMatch, "service_match") <= scoreOf(factorsNoMatch, "service_match") {
		t.Error("matching service should score higher than non-matching service")
	}
}

func TestTotalScore_HighConfidenceThreshold(t *testing.T) {
	s := NewScorer(&fakeHistory{count: 3})
	now := time.Now()

	snap := models.CostSnapshot{Service: "AWSLambda", PeriodStart: now}
	deploy := models.DeployEvent{
		OccurredAt:       now.Add(-30 * time.Minute),
		InferredServices: []string{"AWSLambda"},
		Repository:       "my-lambda-worker",
	}

	factors := s.Score(context.Background(), snap, deploy)
	total := s.TotalScore(factors)

	if total < HighConfidenceThreshold {
		t.Errorf("expected high confidence, got %.2f (threshold=%.2f)", total, HighConfidenceThreshold)
	}
}

func TestTotalScore_WeightsSum(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	now := time.Now()
	snap := models.CostSnapshot{Service: "AmazonEC2"}
	deploy := models.DeployEvent{
		OccurredAt: now.Add(-1 * time.Hour),
	}

	factors := s.Score(context.Background(), snap, deploy)

	var total float64
	for _, f := range factors {
		total += f.Weight
	}

	_ = now
	const want = 1.0
	diff := total - want
	if diff < -0.0001 || diff > 0.0001 {
		t.Errorf("factor weights sum = %.4f, want %.4f", total, want)
	}
}

func TestTemporalScore_DeployDuringPeriod(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	start := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	snap := models.CostSnapshot{
		Service: "AmazonEC2", PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour),
	}

	cases := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"early in period", start.Add(2 * time.Hour), 1.0},
		{"mid period", start.Add(12 * time.Hour), 1.0},
		{"late in period (<25% left)", start.Add(20 * time.Hour), 0.5},
		{"exactly at period end", start.Add(24 * time.Hour), 0.5},
		{"after period end", start.Add(24*time.Hour + time.Minute), 0.0},
		{"before period still decays", start.Add(-3 * time.Hour), 0.85},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.temporalScore(snap, models.DeployEvent{OccurredAt: tc.at}).Score
			if got != tc.want {
				t.Errorf("temporal=%.2f want %.2f", got, tc.want)
			}
		})
	}
}

func factorNamed(t *testing.T, factors []models.ConfidenceFactor, name string) models.ConfidenceFactor {
	t.Helper()
	for _, f := range factors {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no %q factor in %+v", name, factors)
	return models.ConfidenceFactor{}
}

func sumWeights(factors []models.ConfidenceFactor) float64 {
	var w float64
	for _, f := range factors {
		w += f.Weight
	}
	return w
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// With no allocation tags the tag factor used to return a neutral 0.5 at 20%
// weight, adding a constant 0.10 to every score and capping the maximum at 0.90.
// It is now skipped and the other weights are rescaled.
func TestTagFactor_SkippedWithoutTags_WeightsRescaled(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	snap := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: time.Now()}
	deploy := models.DeployEvent{OccurredAt: snap.PeriodStart.Add(-time.Hour), InferredServices: []string{"AWSLambda"}}

	factors := s.Score(context.Background(), snap, deploy)

	tag := factorNamed(t, factors, "tag_match")
	if tag.Weight != 0 || tag.Score != 0 || !strings.Contains(tag.Reason, "not applied") {
		t.Errorf("tag factor = %+v, want it skipped (weight 0) with an explanation", tag)
	}
	if got := sumWeights(factors); !near(got, 1) {
		t.Errorf("weights sum to %v, want 1", got)
	}
	for name, want := range map[string]float64{"temporal_proximity": 0.5, "service_match": 0.375, "historical_pattern": 0.125} {
		if got := factorNamed(t, factors, name).Weight; !near(got, want) {
			t.Errorf("%s weight = %v, want %v (nominal weight / 0.8)", name, got, want)
		}
	}
}

func TestScore_ExactValuesWithoutTags(t *testing.T) {
	cases := []struct {
		name     string
		ago      time.Duration
		services []string
		anomaly  string
		history  int
		want     float64
	}{
		{"≤2h, exact match", time.Hour, []string{"AWSLambda"}, "AWS Lambda", 0, 0.875},
		{"≤6h, exact match", 3 * time.Hour, []string{"AWSLambda"}, "AWS Lambda", 0, 0.80},
		// Previously 0.64, one hundredth under the alert threshold.
		{"≤24h, exact match", 12 * time.Hour, []string{"AWSLambda"}, "AWS Lambda", 0, 0.675},
		{"≤48h, exact match", 36 * time.Hour, []string{"AWSLambda"}, "AWS Lambda", 0, 0.525},
		{"≤2h, partial match", time.Hour, []string{"Kinesis"}, "Amazon Kinesis Firehose", 0, 0.7625},
		{"≤2h, exact match, four prior confirmed blames", time.Hour, []string{"AWSLambda"}, "AWS Lambda", 4, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScorer(&fakeHistory{count: tc.history})
			snap := models.CostSnapshot{Service: tc.anomaly, PeriodStart: time.Now()}
			deploy := models.DeployEvent{OccurredAt: snap.PeriodStart.Add(-tc.ago), InferredServices: tc.services, PRAuthor: "alice"}

			if got := s.TotalScore(s.Score(context.Background(), snap, deploy)); !near(got, tc.want) {
				t.Errorf("total = %.4f, want %.4f", got, tc.want)
			}
		})
	}
}

// The alert tier must keep requiring evidence that the deploy touches the
// spiking service: without a service match even the best possible deploy, with
// maximal history, stays below the threshold.
func TestNoServiceMatchCanNeverReachTheAlertThreshold(t *testing.T) {
	s := NewScorer(&fakeHistory{count: 100}) // history at its cap
	start := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	snap := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour)}

	for _, at := range []time.Time{
		start.Add(10 * time.Hour), start.Add(-time.Hour), start.Add(-5 * time.Hour), start.Add(-20 * time.Hour),
	} {
		deploy := models.DeployEvent{OccurredAt: at, InferredServices: []string{"AmazonS3"}, PRAuthor: "alice"}
		got := s.TotalScore(s.Score(context.Background(), snap, deploy))
		if got >= HighConfidenceThreshold {
			t.Errorf("deploy at %v with no service match scored %.4f, at or above the %.2f alert threshold", at, got, HighConfidenceThreshold)
		}
	}
}

// When tags exist the factor applies at its nominal 20% weight.
func TestTagFactor_AppliedWhenTeamOrEnvTagsPresent(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	now := time.Now()
	deploy := models.DeployEvent{
		OccurredAt: now.Add(-time.Hour), InferredServices: []string{"AWSLambda"},
		Repository: "acme/payments-api", Environment: "prod",
	}

	cases := []struct {
		name      string
		tags      map[string]string
		wantScore float64
	}{
		{"team and env both match", map[string]string{"team": "payments", "env": "prod"}, 1.0},
		{"only the env matches", map[string]string{"team": "billing", "env": "prod"}, 0.5},
		{"neither matches", map[string]string{"team": "billing", "env": "staging"}, 0},
		{"a lone matching team tag", map[string]string{"team": "payments"}, 1.0},
		{"tag keys are matched case-insensitively on values", map[string]string{"team": "PAYMENTS", "env": "Prod"}, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: now, Tags: tc.tags}
			factors := s.Score(context.Background(), snap, deploy)

			tag := factorNamed(t, factors, "tag_match")
			if !near(tag.Score, tc.wantScore) || !near(tag.Weight, 0.2) {
				t.Errorf("tag factor = score %.2f weight %.2f, want score %.2f at the nominal weight 0.20", tag.Score, tag.Weight, tc.wantScore)
			}
			if got := sumWeights(factors); !near(got, 1) {
				t.Errorf("weights sum to %v, want 1", got)
			}
		})
	}
}

func TestTagFactor_SkippedWhenTagsHaveNoTeamOrEnv(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	snap := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: time.Now(), Tags: map[string]string{"cost-center": "1234", "owner": "x"}}
	deploy := models.DeployEvent{OccurredAt: snap.PeriodStart.Add(-time.Hour)}

	factors := s.Score(context.Background(), snap, deploy)
	if tag := factorNamed(t, factors, "tag_match"); tag.Weight != 0 {
		t.Errorf("tags with no team/env cannot be compared; the factor should be skipped, got %+v", tag)
	}
	if got := sumWeights(factors); !near(got, 1) {
		t.Errorf("weights sum to %v, want 1", got)
	}
}

func TestNormalizeWeights(t *testing.T) {
	// Already summing to 1: untouched (no float drift).
	same := []models.ConfidenceFactor{{Weight: 0.4}, {Weight: 0.3}, {Weight: 0.2}, {Weight: 0.1}}
	got := normalizeWeights(append([]models.ConfidenceFactor(nil), same...))
	for i := range same {
		if got[i].Weight != same[i].Weight {
			t.Errorf("factor %d weight changed from %v to %v", i, same[i].Weight, got[i].Weight)
		}
	}
	// All skipped: nothing to divide by.
	zero := normalizeWeights([]models.ConfidenceFactor{{Weight: 0}, {Weight: 0}})
	if zero[0].Weight != 0 || zero[1].Weight != 0 {
		t.Errorf("all-zero weights must stay zero, got %+v", zero)
	}
}
