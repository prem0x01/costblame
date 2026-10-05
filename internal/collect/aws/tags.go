package aws

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/prem0x01/costblame/pkg/models"
)

// Tag attribution.
//
// The scorer's tag factor compares an anomaly's `team` and `env` with the
// deploy's repository and environment, but Cost Explorer rows carry no tags
// unless asked. Splitting every service by tag would put one service into many
// snapshots (colliding snapshot IDs, the same trap as HOURLY) and give each
// team's slice a smaller, noisier baseline than the service as a whole, which
// changes detection sensitivity. So detection stays per service, and tags only
// annotate anomalies: for each anomalous service, the tag value whose spend
// rose the most is attributed to it. That costs one extra Cost Explorer call
// per configured tag, and only when an anomaly exists.
//
// Cost Explorer limits GroupBy to two entries, so this groups by SERVICE plus
// one tag key per call. Cost allocation tags must be activated in the Billing
// console first, or no tag key is returned.

// tagRoles are the roles the scorer understands. tag_keys maps each role to the
// tag key used in the AWS account, e.g. {team: "Team", env: "Environment"}.
var tagRoles = map[string]bool{"team": true, "env": true}

// validateTagKeys rejects a mapping the scorer could not use.
func validateTagKeys(tagKeys map[string]string) error {
	for role, key := range tagKeys {
		if !tagRoles[role] {
			return fmt.Errorf("tag_keys: %q is not a role the scorer uses (want team or env)", role)
		}
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("tag_keys: the AWS tag key for %q must not be empty", role)
		}
	}
	return nil
}

// tagRow is one service's spend on one day for one tag value.
type tagRow struct {
	service string
	value   string
	day     time.Time
	amount  float64
}

// parseTagGroupKey extracts the tag value from a Cost Explorer group key. A TAG
// group key looks like "Team$payments"; spend without the tag comes back as
// "Team$" (and some responses say "Untagged"). Both are reported as untagged.
func parseTagGroupKey(key string) (value string, tagged bool) {
	if i := strings.Index(key, "$"); i >= 0 {
		key = key[i+1:]
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.EqualFold(key, "untagged") {
		return "", false
	}
	return key, true
}

// attributeTags annotates anomalous snapshots with the tag values behind their
// increase. Failures are logged and ignored: tags refine a score but must never
// break cost collection.
func (s *CESource) attributeTags(ctx context.Context, from, to time.Time, snaps []models.CostSnapshot) {
	if len(s.tagKeys) == 0 {
		return
	}
	anomalous := map[string]bool{}
	for _, snap := range snaps {
		if snap.IsAnomaly {
			anomalous[snap.Service] = true
		}
	}
	if len(anomalous) == 0 {
		return
	}

	roles := make([]string, 0, len(s.tagKeys))
	for role := range s.tagKeys {
		roles = append(roles, role)
	}
	sort.Strings(roles) // a stable call order

	prevFrom := from.Add(-to.Sub(from))
	for _, role := range roles {
		rows, err := s.fetchTagCosts(ctx, prevFrom, to, s.tagKeys[role])
		if err != nil {
			slog.Warn("aws: could not attribute a tag to the anomalies; tag_match will be skipped",
				"role", role, "tag_key", s.tagKeys[role], "err", err)
			continue
		}
		winners := topIncrease(rows, from)
		for i := range snaps {
			if !snaps[i].IsAnomaly {
				continue
			}
			if value, ok := winners[snaps[i].Service]; ok {
				if snaps[i].Tags == nil {
					snaps[i].Tags = map[string]string{}
				}
				snaps[i].Tags[role] = value
			}
		}
	}
}

// topIncrease returns, per service, the tagged value whose spend rose most from
// the period before `from` to the period starting at `from`. Services where no
// tagged value rose, or where untagged spend is the main driver, get no entry:
// an unknown owner is better reported as unknown than guessed.
func topIncrease(rows []tagRow, from time.Time) map[string]string {
	type key struct{ service, value string }
	cur := map[key]float64{}
	prev := map[key]float64{}
	services := map[string]bool{}
	for _, r := range rows {
		k := key{r.service, r.value}
		services[r.service] = true
		if r.day.Before(from) {
			prev[k] += r.amount
		} else {
			cur[k] += r.amount
		}
	}

	out := map[string]string{}
	for svc := range services {
		var bestValue string
		best := 0.0
		untaggedRise := 0.0
		seen := map[string]bool{}
		for k := range cur {
			if k.service == svc {
				seen[k.value] = true
			}
		}
		for k := range prev {
			if k.service == svc {
				seen[k.value] = true
			}
		}
		values := make([]string, 0, len(seen))
		for v := range seen {
			values = append(values, v)
		}
		sort.Strings(values) // ties resolve alphabetically, so results are stable
		for _, v := range values {
			rise := cur[key{svc, v}] - prev[key{svc, v}]
			if v == "" {
				untaggedRise = rise
				continue
			}
			if rise > best {
				best, bestValue = rise, v
			}
		}
		if bestValue != "" && best > untaggedRise {
			out[svc] = bestValue
		}
	}
	return out
}

// fetchTagCosts returns daily spend per (service, tag value) over [from, to).
func (s *CESource) fetchTagCosts(ctx context.Context, from, to time.Time, tagKey string) ([]tagRow, error) {
	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(from.Format("2006-01-02")),
			End:   aws.String(to.Format("2006-01-02")),
		},
		Granularity: cetypes.GranularityDaily,
		GroupBy: []cetypes.GroupDefinition{
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")},
			{Type: cetypes.GroupDefinitionTypeTag, Key: aws.String(tagKey)},
		},
		Metrics: []string{"UnblendedCost"},
	}

	var rows []tagRow
	for {
		out, err := s.client.GetCostAndUsage(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, result := range out.ResultsByTime {
			day := parseBucketStart(result.TimePeriod)
			for _, g := range result.Groups {
				if len(g.Keys) < 2 {
					continue
				}
				value, _ := parseTagGroupKey(g.Keys[1])
				amount := parseAmount(g.Metrics["UnblendedCost"].Amount)
				rows = append(rows, tagRow{service: g.Keys[0], value: value, day: day, amount: amount})
			}
		}
		if out.NextPageToken == nil {
			break
		}
		input.NextPageToken = out.NextPageToken
	}
	return rows, nil
}
