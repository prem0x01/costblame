package aws

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/prem0x01/costblame/pkg/models"
)

const sourceName = "aws"

// costAPI is the slice of the Cost Explorer client the adapter uses; it lets
// tests substitute a fake without AWS credentials or network access.
type costAPI interface {
	GetCostAndUsage(ctx context.Context, params *costexplorer.GetCostAndUsageInput,
		optFns ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
}

// CESource polls AWS Cost Explorer and emits CostSnapshot records.
// It groups costs by SERVICE and any configured tag keys.
type CESource struct {
	client       costAPI
	region       string
	granularity  cetypes.Granularity
	lookbackDays int
	detector     Detector
}

// NewCESource constructs a CESource using the default AWS credential chain.
// detector's settings are validated here so a bad value fails at startup.
func NewCESource(ctx context.Context, region string, granularity string, lookbackDays int, detector Detector) (*CESource, error) {
	if err := detector.validate(lookbackDays); err != nil {
		return nil, err
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var gran cetypes.Granularity
	if granularity == "HOURLY" {
		gran = cetypes.GranularityHourly
	} else {
		gran = cetypes.GranularityDaily
	}

	return &CESource{
		client:       costexplorer.NewFromConfig(cfg),
		region:       region,
		granularity:  gran,
		lookbackDays: lookbackDays,
		detector:     detector,
	}, nil
}

func (s *CESource) Name() string { return sourceName }

// Collect fetches per-service costs for [from, to] and builds CostSnapshot records.
// It also fetches the preceding equivalent window to compute deltas and detect anomalies.
func (s *CESource) Collect(ctx context.Context, from, to time.Time) ([]models.CostSnapshot, error) {
	periodLen := to.Sub(from)
	baselineFrom := from.Add(-time.Duration(s.lookbackDays) * 24 * time.Hour)
	baselineTo := from

	// Fetch current and baseline periods in parallel.
	type result struct {
		rows []ceRow
		err  error
	}
	curCh := make(chan result, 1)
	baseCh := make(chan result, 1)

	go func() {
		rows, err := s.fetchCosts(ctx, from, to)
		curCh <- result{rows, err}
	}()
	go func() {
		rows, err := s.fetchCosts(ctx, baselineFrom, baselineTo)
		baseCh <- result{rows, err}
	}()

	curResult := <-curCh
	baseResult := <-baseCh
	if curResult.err != nil {
		return nil, fmt.Errorf("fetching current costs: %w", curResult.err)
	}
	if baseResult.err != nil {
		return nil, fmt.Errorf("fetching baseline costs: %w", baseResult.err)
	}

	// One observation per service per baseline day; days with no billing row are
	// zero, so intermittent services are described accurately.
	history := buildSeries(baseResult.rows, baselineFrom, baselineTo, s.granularity == cetypes.GranularityDaily)

	// Build the previous-period total for delta calculation.
	prevPeriodFrom := from.Add(-periodLen)
	prevRows, err := s.fetchCosts(ctx, prevPeriodFrom, from)
	if err != nil {
		return nil, fmt.Errorf("fetching previous period costs: %w", err)
	}
	prevMap := buildTotalMap(prevRows)

	var snapshots []models.CostSnapshot
	learning := 0
	for _, row := range curResult.rows {
		prev := prevMap[row.service]
		verdict := s.detector.Assess(history[row.service], row.day, row.amount, prev)
		if verdict.Learning {
			learning++
		}

		var deltaPct float64
		if prev > 0 {
			deltaPct = ((row.amount - prev) / prev) * 100
		}

		gran := models.Granularity(s.granularity)
		snapshots = append(snapshots, models.CostSnapshot{
			ID:            models.SnapshotID(sourceName, row.service, from, to, gran),
			CollectedAt:   time.Now().UTC(),
			PeriodStart:   from,
			PeriodEnd:     to,
			Source:        sourceName,
			Service:       row.service,
			Region:        s.region,
			Tags:          row.tags,
			AmountUSD:     row.amount,
			PrevAmountUSD: prev,
			DeltaUSD:      row.amount - prev,
			DeltaPct:      deltaPct,
			IsAnomaly:     verdict.Anomaly,
			AnomalyScore:  verdict.Z,
			Granularity:   gran,
		})
	}

	if learning > 0 {
		slog.Debug("aws: services with too little history to judge, not flagged",
			"services", learning, "min_history_days", s.detector.MinHistory)
	}
	return snapshots, nil
}

// ceRow is a raw row returned by Cost Explorer before enrichment.
type ceRow struct {
	day     time.Time // start of the result's time bucket (UTC)
	service string
	tags    map[string]string
	amount  float64
}

func (s *CESource) fetchCosts(ctx context.Context, from, to time.Time) ([]ceRow, error) {
	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(from.Format("2006-01-02")),
			End:   aws.String(to.Format("2006-01-02")),
		},
		Granularity: s.granularity,
		GroupBy: []cetypes.GroupDefinition{
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")},
		},
		Metrics: []string{"UnblendedCost"},
	}

	var rows []ceRow
	for {
		output, err := s.client.GetCostAndUsage(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, result := range output.ResultsByTime {
			for _, group := range result.Groups {
				if len(group.Keys) == 0 {
					continue
				}
				svc := group.Keys[0]
				amountStr := aws.ToString(group.Metrics["UnblendedCost"].Amount)
				amount, _ := strconv.ParseFloat(amountStr, 64)
				rows = append(rows, ceRow{
					day:     parseBucketStart(result.TimePeriod),
					service: svc,
					tags:    map[string]string{},
					amount:  amount,
				})
			}
		}
		if output.NextPageToken == nil {
			break
		}
		input.NextPageToken = output.NextPageToken
	}

	return rows, nil
}

// parseBucketStart reads the start of a Cost Explorer time bucket: a plain date
// for DAILY results, a full timestamp for HOURLY ones. Unparseable input yields
// the zero time rather than an error, since the day only refines the baseline.
func parseBucketStart(p *cetypes.DateInterval) time.Time {
	if p == nil {
		return time.Time{}
	}
	start := aws.ToString(p.Start)
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, start); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// buildSeries groups rows by service into a per-day history over [from, to).
// With daily set, every day in the window gets an observation and a missing
// day counts as zero spend; otherwise rows are used as returned.
func buildSeries(rows []ceRow, from, to time.Time, daily bool) map[string][]observation {
	out := map[string][]observation{}
	if !daily {
		for _, r := range rows {
			out[r.service] = append(out[r.service], observation{Day: r.day, Amount: r.amount})
		}
		return out
	}

	start := from.UTC().Truncate(24 * time.Hour)
	end := to.UTC().Truncate(24 * time.Hour)
	spend := map[string]map[time.Time]float64{}
	for _, r := range rows {
		if spend[r.service] == nil {
			spend[r.service] = map[time.Time]float64{}
		}
		spend[r.service][r.day.UTC().Truncate(24*time.Hour)] += r.amount
	}
	for svc, byDay := range spend {
		for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
			out[svc] = append(out[svc], observation{Day: d, Amount: byDay[d]})
		}
	}
	return out
}

func buildTotalMap(rows []ceRow) map[string]float64 {
	m := map[string]float64{}
	for _, r := range rows {
		m[r.service] += r.amount
	}
	return m
}
