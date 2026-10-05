package correlate

import (
	"context"
	"testing"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

func TestEnvClass(t *testing.T) {
	cases := map[string]string{
		// production, however it is spelled
		"prod": EnvProd, "Prod": EnvProd, "production": EnvProd, "Production": EnvProd, "PRD": EnvProd,
		"live": EnvProd, "prod-eu": EnvProd, "prod_us_east": EnvProd, "payments-prod": EnvProd, "prod2": EnvProd,
		"eu-production": EnvProd,
		// not production, even though some contain "prod"
		"staging": EnvStaging, "stage": EnvStaging, "stg": EnvStaging, "preprod": EnvStaging, "pre-prod": EnvStaging,
		"pre-production": EnvStaging, "non-prod": EnvStaging, "nonprod": EnvStaging, "uat": EnvStaging,
		"pr-482-preview": EnvStaging, "staging-production-like": EnvStaging,
		"dev": EnvDev, "development": EnvDev, "sandbox": EnvDev,
		"test": EnvTest, "qa": EnvTest,
		// cannot tell: must be skipped, not treated as a mismatch
		"": "", "unknown": "", "payments": "", "default": "", "kube-system": "", "product-docs": "",
	}
	for in, want := range cases {
		if got := EnvClass(in); got != want {
			t.Errorf("EnvClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeployTeamNames(t *testing.T) {
	cases := []struct {
		name   string
		deploy models.DeployEvent
		want   []string // every name must be present
	}{
		{"owner/repo", models.DeployEvent{Repository: "acme/payments-api"}, []string{"payments"}},
		{"suffixes stripped", models.DeployEvent{Repository: "acme/billing-service"}, []string{"billing"}},
		{"nested GitLab path: the last segment, not 'sub/payments-api'", models.DeployEvent{Repository: "grp/sub/payments-api"}, []string{"payments"}},
		{"ArgoCD repo URL", models.DeployEvent{Repository: "https://git.example/acme/payments-worker.git"}, []string{"payments"}},
		{"scp-style URL", models.DeployEvent{Repository: "git@github.com:acme/checkout-payments.git"}, []string{"checkout-payments", "checkout", "payments"}},
		{"an explicit team label", models.DeployEvent{PRTeam: "Platform", Repository: "acme/x-api"}, []string{"Platform"}},
	}
	for _, tc := range cases {
		got := deployTeamNames(tc.deploy)
		for _, w := range tc.want {
			found := false
			for _, g := range got {
				if g == w {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: names %v are missing %q", tc.name, got, w)
			}
		}
	}
	if got := deployTeamNames(models.DeployEvent{}); len(got) != 0 {
		t.Errorf("a deploy with no repository and no team has no names, got %v", got)
	}
}

func TestTeamMatches(t *testing.T) {
	d := models.DeployEvent{Repository: "acme/checkout-payments-api"}
	for team, want := range map[string]bool{
		"checkout-payments": true, "Checkout-Payments": true, "payments": true, "checkout": true, // a word of the name
		"Checkout Payments": true,                           // spacing and punctuation are ignored
		"billing":           false, "": false, "pay": false, // too short to count as a word match
	} {
		if got := teamMatches(team, d); got != want {
			t.Errorf("teamMatches(%q) = %v, want %v", team, got, want)
		}
	}
}

// The core promise: if the tags attribute the spike to the right team and
// environment, they must never cost a correct alert its score just because the
// deploy source spells things differently.
func TestRightTagsNeverCostACorrectAlertToSpelling(t *testing.T) {
	anomalyTags := map[string]string{"team": "Payments", "env": "Production"} // as an AWS account might write them
	flavours := []struct {
		name   string
		deploy models.DeployEvent
	}{
		{"GitHub workflow_run on main", models.DeployEvent{Repository: "Acme/Payments-API", Environment: "prod"}},
		{"GitHub deployment_status", models.DeployEvent{Repository: "acme/payments-api", Environment: "production"}},
		{"GitHub deploy/* branch (environment unknown)", models.DeployEvent{Repository: "acme/payments-api", Environment: "unknown"}},
		{"GitLab, nested group, hard-coded production", models.DeployEvent{Repository: "grp/sub/payments-api", Environment: "production"}},
		{"ArgoCD, namespace is not an environment name", models.DeployEvent{Repository: "https://git.example/acme/infra.git", Environment: "payments"}},
		{"ArgoCD, namespace names the environment", models.DeployEvent{Repository: "https://git.example/acme/infra.git", Environment: "payments-prod"}},
		{"nothing known about the deploy", models.DeployEvent{}},
	}
	s := NewScorer(&fakeHistory{})
	start := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	for _, f := range flavours {
		t.Run(f.name, func(t *testing.T) {
			f.deploy.OccurredAt = start.Add(-time.Hour)
			f.deploy.InferredServices = []string{"AWSLambda"}
			plain := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour)}
			tagged := plain
			tagged.Tags = anomalyTags

			without := s.TotalScore(s.Score(context.Background(), plain, f.deploy))
			with := s.TotalScore(s.Score(context.Background(), tagged, f.deploy))

			if with < without-1e-9 {
				t.Errorf("correct tags lowered the score from %.4f to %.4f", without, with)
			}
			if with < HighConfidenceThreshold {
				t.Errorf("a correct alert (%.4f) fell below the threshold with tags on", with)
			}
		})
	}
}

// A confirmed environment mismatch is real evidence, unlike a vocabulary gap.
func TestEnvironmentMismatchStillCounts(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	start := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	anomaly := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: start, Tags: map[string]string{"env": "Production"}}
	staging := models.DeployEvent{OccurredAt: start.Add(-time.Hour), InferredServices: []string{"AWSLambda"}, Environment: "staging"}
	prod := models.DeployEvent{OccurredAt: start.Add(-time.Hour), InferredServices: []string{"AWSLambda"}, Environment: "prod"}

	stagingScore := s.TotalScore(s.Score(context.Background(), anomaly, staging))
	prodScore := s.TotalScore(s.Score(context.Background(), anomaly, prod))

	if stagingScore >= prodScore {
		t.Errorf("a staging deploy (%.3f) must rank below a prod deploy (%.3f) for production spend", stagingScore, prodScore)
	}
	if tag := factorNamed(t, s.Score(context.Background(), anomaly, staging), "tag_match"); tag.Weight == 0 || tag.Score != 0 {
		t.Errorf("a staging/production mismatch should apply at score 0, got %+v", tag)
	}
}

func TestTagFactor_SkippedWhenNothingIsComparable(t *testing.T) {
	s := NewScorer(&fakeHistory{})
	start := time.Now()
	anomaly := models.CostSnapshot{Service: "AWS Lambda", PeriodStart: start, Tags: map[string]string{"team": "billing", "env": "prod"}}
	// The environment is unknown and the team does not match: nothing can be judged.
	deploy := models.DeployEvent{OccurredAt: start.Add(-time.Hour), Repository: "acme/payments-api", Environment: "unknown"}

	factors := s.Score(context.Background(), anomaly, deploy)
	if tag := factorNamed(t, factors, "tag_match"); tag.Weight != 0 {
		t.Errorf("tag factor = %+v, want it skipped", tag)
	}
	if got := sumWeights(factors); !near(got, 1) {
		t.Errorf("weights sum to %v, want 1", got)
	}
}
