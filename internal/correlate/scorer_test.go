package correlate

import (
	"testing"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

type fakeHistory struct{ count int }

func (f *fakeHistory) ConfirmedBlameCount(_, _ string) int { return f.count }

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
		factors := s.Score(snap, deploy)

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

	factorsMatch := s.Score(matchSnap, deploy)
	factorsNoMatch := s.Score(noMatchSnap, deploy)

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

	factors := s.Score(snap, deploy)
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

	factors := s.Score(snap, deploy)

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
