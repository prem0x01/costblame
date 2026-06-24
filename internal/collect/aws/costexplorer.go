package aws

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

const sourceName = "aws"

// zScoreAnomalyThreshold: flag costs that are more than 2 stddev above baseline.
const zScoreAnomalyThreshold = 2.0

// CESource polls AWS Cost Explorer and emits CostSnapshot records.
// It groups costs by SERVICE and any configured tag keys.
type CESource struct {
	client       *costexplorer.Client
	region       string
	granularity  cetypes.Granularity
	lookbackDays int
	minDeltaPct  float64
}

// NewCESource constructs a CESource using the default AWS credential chain.
func NewCESource(ctx context.Context, region string, granularity string, lookbackDays int, minDeltaPct float64) (*CESource, error) {
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
		minDeltaPct:  minDeltaPct,
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

	// Build a baseline map: service → list of daily amounts.
	baselineMap := buildBaselineMap(baseResult.rows)

	// Build the previous-period total for delta calculation.
	prevPeriodFrom := from.Add(-periodLen)
	prevRows, err := s.fetchCosts(ctx, prevPeriodFrom, from)
	if err != nil {
		return nil, fmt.Errorf("fetching previous period costs: %w", err)
	}
	prevMap := buildTotalMap(prevRows)

	var snapshots []models.CostSnapshot
	for _, row := range curResult.rows {
		prev := prevMap[row.service]
		base := newBaseline(baselineMap[row.service])
		anomaly, zScore := base.isAnomaly(row.amount, prev, s.minDeltaPct, zScoreAnomalyThreshold)

		var deltaPct float64
		if prev > 0 {
			deltaPct = ((row.amount - prev) / prev) * 100
		}

		snapshots = append(snapshots, models.CostSnapshot{
			ID:            uuid.New(),
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
			IsAnomaly:     anomaly,
			AnomalyScore:  zScore,
			Granularity:   models.Granularity(s.granularity),
		})
	}

	return snapshots, nil
}

// ceRow is a raw row returned by Cost Explorer before enrichment.
type ceRow struct {
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

func buildBaselineMap(rows []ceRow) map[string][]float64 {
	m := map[string][]float64{}
	for _, r := range rows {
		m[r.service] = append(m[r.service], r.amount)
	}
	return m
}

func buildTotalMap(rows []ceRow) map[string]float64 {
	m := map[string]float64{}
	for _, r := range rows {
		m[r.service] += r.amount
	}
	return m
}
