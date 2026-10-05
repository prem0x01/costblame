package aws

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

// The real client must keep satisfying the interface the adapter depends on.
var _ costAPI = (*costexplorer.Client)(nil)

// fakeCE answers GetCostAndUsage from a per-service, per-day spend function,
// returning one daily bucket per day in the requested range. Collect issues
// requests concurrently, so the call counter is atomic.
type fakeCE struct {
	services []string
	spend    func(service string, day time.Time) (float64, bool)
	calls    atomic.Int64
}

func (f *fakeCE) GetCostAndUsage(_ context.Context, in *costexplorer.GetCostAndUsageInput,
	_ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	f.calls.Add(1)
	start, err := time.Parse("2006-01-02", aws.ToString(in.TimePeriod.Start))
	if err != nil {
		return nil, err
	}
	end, err := time.Parse("2006-01-02", aws.ToString(in.TimePeriod.End))
	if err != nil {
		return nil, err
	}

	var results []cetypes.ResultByTime
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		var groups []cetypes.Group
		for _, svc := range f.services {
			if amt, ok := f.spend(svc, d); ok {
				groups = append(groups, cetypes.Group{
					Keys:    []string{svc},
					Metrics: map[string]cetypes.MetricValue{"UnblendedCost": {Amount: aws.String(fmt.Sprintf("%.2f", amt))}},
				})
			}
		}
		results = append(results, cetypes.ResultByTime{
			TimePeriod: &cetypes.DateInterval{
				Start: aws.String(d.Format("2006-01-02")),
				End:   aws.String(d.AddDate(0, 0, 1).Format("2006-01-02")),
			},
			Groups: groups,
		})
	}
	return &costexplorer.GetCostAndUsageOutput{ResultsByTime: results}, nil
}

func TestParseBucketStart(t *testing.T) {
	want := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	for name, in := range map[string]*cetypes.DateInterval{
		"date":      {Start: aws.String("2026-07-08")},
		"timestamp": {Start: aws.String("2026-07-08T00:00:00Z")},
	} {
		if got := parseBucketStart(in); !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	for name, in := range map[string]*cetypes.DateInterval{
		"nil": nil, "garbage": {Start: aws.String("not a date")}, "empty": {},
	} {
		if got := parseBucketStart(in); !got.IsZero() {
			t.Errorf("%s: got %v, want the zero time", name, got)
		}
	}
}

func TestBuildSeries_ZeroFillsMissingDaysAndSumsDuplicates(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 10)
	rows := []ceRow{
		{service: "AmazonEC2", day: from.AddDate(0, 0, 2), amount: 4},
		{service: "AmazonEC2", day: from.AddDate(0, 0, 2), amount: 6}, // same day twice: summed
		{service: "AmazonEC2", day: from.AddDate(0, 0, 7), amount: 1},
		{service: "AmazonS3", day: from, amount: 2},
	}

	got := buildSeries(rows, from, to, true)
	if len(got["AmazonEC2"]) != 10 || len(got["AmazonS3"]) != 10 {
		t.Fatalf("series lengths = %d, %d; want 10 each (one per day, gaps filled)", len(got["AmazonEC2"]), len(got["AmazonS3"]))
	}
	if got["AmazonEC2"][2].Amount != 10 {
		t.Errorf("duplicate rows for one day = %v, want summed 10", got["AmazonEC2"][2].Amount)
	}
	if got["AmazonEC2"][0].Amount != 0 || got["AmazonEC2"][5].Amount != 0 {
		t.Error("missing days should be zero")
	}
	if got["AmazonEC2"][9].Day != from.AddDate(0, 0, 9) {
		t.Errorf("last observation day = %v", got["AmazonEC2"][9].Day)
	}
}

func TestBuildSeries_NonDailyKeepsRowsAsReturned(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rows := []ceRow{{service: "AmazonEC2", day: from, amount: 1}, {service: "AmazonEC2", day: from.Add(time.Hour), amount: 2}}
	if got := buildSeries(rows, from, from.AddDate(0, 0, 1), false); len(got["AmazonEC2"]) != 2 {
		t.Errorf("hourly rows were altered: %v", got)
	}
}

// End to end through Collect with a fake Cost Explorer.
func TestCollect_DetectsSpikeOnFlatBaselineAndSkipsNewServices(t *testing.T) {
	from := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)

	fake := &fakeCE{
		services: []string{"AmazonEC2", "AmazonRDS", "AWSLambda", "AmazonS3"},
		spend: func(svc string, day time.Time) (float64, bool) {
			spike := day.Equal(from)
			switch svc {
			case "AmazonEC2": // flat $5/day, then $500 on the day being collected
				if spike {
					return 500, true
				}
				return 5, true
			case "AmazonRDS": // flat $50/day, unchanged
				return 50, true
			case "AWSLambda": // brand-new service: only the last two days exist
				if !day.Before(from.AddDate(0, 0, -1)) {
					return 300, true
				}
				return 0, false
			case "AmazonS3": // billed on some days only, $10 whenever it runs
				if spike || day.YearDay()%4 == 0 {
					return 10, true
				}
				return 0, false
			}
			return 0, false
		},
	}
	src := &CESource{client: fake, region: "us-east-1", granularity: cetypes.GranularityDaily, lookbackDays: 30, detector: testDetector()}

	snaps, err := src.Collect(context.Background(), from, to)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	by := map[string]bool{}
	score := map[string]float64{}
	for _, s := range snaps {
		by[s.Service] = s.IsAnomaly
		score[s.Service] = s.AnomalyScore
	}

	if !by["AmazonEC2"] {
		t.Errorf("EC2 jumped $5 -> $500 on a flat baseline but was not flagged (z=%.1f)", score["AmazonEC2"])
	}
	if score["AmazonEC2"] <= 3.5 {
		t.Errorf("EC2 z-score = %.1f, want above the 3.5 threshold", score["AmazonEC2"])
	}
	if by["AmazonRDS"] {
		t.Error("an unchanged service was flagged")
	}
	if by["AWSLambda"] {
		t.Error("a two-day-old service has no baseline yet and must not be flagged (still learning)")
	}
	if by["AmazonS3"] {
		t.Errorf("an intermittent service running as usual was flagged (z=%.1f)", score["AmazonS3"])
	}
	if len(snaps) != 4 {
		t.Errorf("snapshots = %d, want 4 (one per service)", len(snaps))
	}
}
