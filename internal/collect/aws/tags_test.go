package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

func TestParseTagGroupKey(t *testing.T) {
	cases := []struct {
		in         string
		wantValue  string
		wantTagged bool
	}{
		{"Team$payments", "payments", true},
		{"Environment$prod", "prod", true},
		{"Team$", "", false},         // spend without the tag
		{"Team$Untagged", "", false}, // some responses spell it out
		{"Team$untagged", "", false},
		{"Team$ ", "", false},
		{"", "", false},
		{"payments", "payments", true}, // no "$": use the key as is
		{"Team$a$b", "a$b", true},      // only the first "$" separates key from value
	}
	for _, tc := range cases {
		v, ok := parseTagGroupKey(tc.in)
		if v != tc.wantValue || ok != tc.wantTagged {
			t.Errorf("parseTagGroupKey(%q) = (%q, %v), want (%q, %v)", tc.in, v, ok, tc.wantValue, tc.wantTagged)
		}
	}
}

var tagDay = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)

func rows(service string, prevSpend, curSpend map[string]float64) []tagRow {
	var out []tagRow
	for v, a := range prevSpend {
		out = append(out, tagRow{service: service, value: v, day: tagDay.AddDate(0, 0, -1), amount: a})
	}
	for v, a := range curSpend {
		out = append(out, tagRow{service: service, value: v, day: tagDay, amount: a})
	}
	return out
}

func TestTopIncrease(t *testing.T) {
	cases := []struct {
		name string
		rows []tagRow
		want string // "" = no attribution
	}{
		{"the biggest increase wins, not the biggest spender",
			rows("EC2", map[string]float64{"data": 900, "payments": 5}, map[string]float64{"data": 905, "payments": 400}), "payments"},
		{"a value that only appears today",
			rows("EC2", map[string]float64{"data": 10}, map[string]float64{"data": 10, "payments": 300}), "payments"},
		{"nothing rose: no attribution",
			rows("EC2", map[string]float64{"data": 100}, map[string]float64{"data": 90}), ""},
		{"untagged spend is the main driver: unknown is not guessed",
			rows("EC2", map[string]float64{"": 5, "data": 5}, map[string]float64{"": 500, "data": 20}), ""},
		{"a tagged value beats a smaller untagged rise",
			rows("EC2", map[string]float64{"": 5, "payments": 5}, map[string]float64{"": 50, "payments": 500}), "payments"},
		{"exact tie resolves alphabetically (stable)",
			rows("EC2", map[string]float64{"b": 1, "a": 1}, map[string]float64{"b": 101, "a": 101}), "a"},
		{"no rows", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := topIncrease(tc.rows, tagDay)["EC2"]
			if got != tc.want {
				t.Errorf("winner = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTopIncrease_ServicesAreIndependent(t *testing.T) {
	r := append(
		rows("EC2", map[string]float64{"data": 1}, map[string]float64{"data": 100}),
		rows("AWS Lambda", map[string]float64{"payments": 1}, map[string]float64{"payments": 100})...)
	got := topIncrease(r, tagDay)
	if got["EC2"] != "data" || got["AWS Lambda"] != "payments" {
		t.Errorf("winners = %v", got)
	}
}

func TestValidateTagKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		keys    map[string]string
		wantErr string
	}{
		"nil is valid": {nil, ""},
		"team and env": {map[string]string{"team": "Team", "env": "Environment"}, ""},
		"unknown role": {map[string]string{"owner": "Owner"}, "not a role"},
		"empty key":    {map[string]string{"team": "  "}, "must not be empty"},
	} {
		err := validateTagKeys(tc.keys)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.wantErr)
		}
	}
}

// collectFixture: EC2 jumps from $5 to $500 on the collected day; RDS is steady.
func collectFixture(tagKeys map[string]string, tagSpend func(key, svc string, day time.Time) map[string]float64) (*CESource, *fakeCE, time.Time, time.Time) {
	from := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	fake := &fakeCE{
		services: []string{"Amazon Elastic Compute Cloud - Compute", "Amazon Relational Database Service"},
		spend: func(svc string, day time.Time) (float64, bool) {
			if strings.Contains(svc, "Compute") && day.Equal(from) {
				return 500, true
			}
			if strings.Contains(svc, "Compute") {
				return 5, true
			}
			return 50, true
		},
		tagSpend: tagSpend,
	}
	src := &CESource{client: fake, region: "us-east-1", granularity: cetypes.GranularityDaily,
		lookbackDays: 30, detector: testDetector(), tagKeys: tagKeys}
	return src, fake, from, from.AddDate(0, 0, 1)
}

func TestCollect_AttributesTagsToAnomalousServices(t *testing.T) {
	src, fake, from, to := collectFixture(
		map[string]string{"team": "Team", "env": "Environment"},
		func(key, svc string, day time.Time) map[string]float64 {
			if !strings.Contains(svc, "Compute") {
				return map[string]float64{"payments": 50}
			}
			spike := day.Equal(from0)
			switch key {
			case "Team": // payments' spend explodes; data is bigger but flat
				if spike {
					return map[string]float64{"payments": 400, "data": 100}
				}
				return map[string]float64{"payments": 5, "data": 100}
			default: // Environment: prod rises, staging is flat
				if spike {
					return map[string]float64{"prod": 450, "staging": 50}
				}
				return map[string]float64{"prod": 5, "staging": 50}
			}
		})

	snaps, err := src.Collect(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	var ec2, rds = -1, -1
	for i, s := range snaps {
		if strings.Contains(s.Service, "Compute") {
			ec2 = i
		} else {
			rds = i
		}
	}
	if !snaps[ec2].IsAnomaly {
		t.Fatal("setup: EC2 should be anomalous")
	}
	if got := snaps[ec2].Tags; got["team"] != "payments" || got["env"] != "prod" {
		t.Errorf("EC2 tags = %v, want team=payments (largest rise, not largest spender) and env=prod", got)
	}
	if len(snaps[rds].Tags) != 0 {
		t.Errorf("a non-anomalous service must not be tagged, got %v", snaps[rds].Tags)
	}
	if got := fake.tagCalls.Load(); got != 2 {
		t.Errorf("tag queries = %d, want one per configured role", got)
	}
	// The AWS tag keys from config are what is requested, not the role names.
	keys := strings.Join(fake.tagKeys, ",")
	if keys != "Environment,Team" { // roles are queried in sorted order: env, team
		t.Errorf("requested tag keys = %q, want Environment,Team", keys)
	}
	// Detection itself is unchanged by tags: still one snapshot per service.
	if len(snaps) != 2 {
		t.Errorf("snapshots = %d, want 2 (one per service, not per tag value)", len(snaps))
	}
}

var from0 = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)

func TestCollect_NoTagKeysMeansNoExtraCalls(t *testing.T) {
	src, fake, from, to := collectFixture(nil, func(string, string, time.Time) map[string]float64 {
		t.Error("tag data requested although no tag_keys are configured")
		return nil
	})
	snaps, err := src.Collect(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if fake.tagCalls.Load() != 0 || fake.calls.Load() != 3 {
		t.Errorf("calls = %d (%d tag), want exactly the 3 detection calls", fake.calls.Load(), fake.tagCalls.Load())
	}
	for _, s := range snaps {
		if len(s.Tags) != 0 {
			t.Errorf("unexpected tags %v", s.Tags)
		}
	}
}

func TestCollect_NoAnomalyMeansNoTagCalls(t *testing.T) {
	src, fake, from, to := collectFixture(map[string]string{"team": "Team"},
		func(string, string, time.Time) map[string]float64 { return map[string]float64{"payments": 1} })
	src.client = &fakeCE{ // everything steady: nothing is anomalous
		services: []string{"Amazon Elastic Compute Cloud - Compute"},
		spend:    func(string, time.Time) (float64, bool) { return 5, true },
		tagSpend: func(string, string, time.Time) map[string]float64 { return map[string]float64{"payments": 5} },
	}
	_ = fake
	if _, err := src.Collect(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	if got := src.client.(*fakeCE).tagCalls.Load(); got != 0 {
		t.Errorf("tag queries = %d with no anomaly, want 0 (they cost money)", got)
	}
}

// Tags refine a score; a failing tag query must never break cost collection.
func TestCollect_TagQueryFailureIsNotFatal(t *testing.T) {
	src, fake, from, to := collectFixture(map[string]string{"team": "Team"},
		func(string, string, time.Time) map[string]float64 { return nil })
	fake.tagErr = errors.New("AccessDenied: ce:GetCostAndUsage with tags")

	snaps, err := src.Collect(context.Background(), from, to)
	if err != nil {
		t.Fatalf("a tag failure must not fail Collect: %v", err)
	}
	anomalous := false
	for _, s := range snaps {
		if s.IsAnomaly {
			anomalous = true
		}
		if len(s.Tags) != 0 {
			t.Errorf("tags set despite the failure: %v", s.Tags)
		}
	}
	if !anomalous {
		t.Error("detection must still work when tags fail")
	}
}

func TestCollect_UntaggedSpendIsNotAttributed(t *testing.T) {
	src, _, from, to := collectFixture(map[string]string{"team": "Team"},
		func(key, svc string, day time.Time) map[string]float64 {
			if !strings.Contains(svc, "Compute") {
				return nil
			}
			if day.Equal(from0) {
				return map[string]float64{"": 480, "payments": 20} // the rise is untagged spend
			}
			return map[string]float64{"": 5, "payments": 5}
		})
	snaps, _ := src.Collect(context.Background(), from, to)
	for _, s := range snaps {
		if len(s.Tags) != 0 {
			t.Errorf("an unknown owner must stay unknown, got %v", s.Tags)
		}
	}
}
