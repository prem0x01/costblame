package aws

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	// madScale makes the median absolute deviation comparable to a standard
	// deviation for normally distributed data, so a "z-score" computed from it
	// reads like the familiar kind.
	madScale = 1.4826

	// minSigmaUSD guards the division when both the history and the configured
	// floor are zero. One cent is below any amount worth reporting.
	minSigmaUSD = 0.01

	// minSameWeekdaySamples is how many same-weekday observations the
	// weekday-aware baseline needs before it replaces the all-days baseline.
	minSameWeekdaySamples = 4
)

// Detector decides whether one service's spend for one day is anomalous
// relative to its recent history.
//
// It uses the median and the median absolute deviation (MAD) instead of the
// mean and standard deviation. Those are robust: a single past spike, or a
// handful of them, does not inflate the baseline and hide the next one. A floor
// on the spread makes a perfectly flat history (the classic $5/day, then $500)
// detectable, where a plain standard deviation of zero divides to nothing.
type Detector struct {
	// ZThreshold is the robust z-score above which spend counts as anomalous.
	// 3.5 is the conventional cut-off for MAD-based scores (Iglewicz & Hoaglin).
	ZThreshold float64
	// MinDeltaPct also requires the day-over-day increase to be at least this
	// large, filtering "statistically unusual but who cares" noise. It also sets
	// the relative floor on the spread (see relativeFloor).
	MinDeltaPct float64
	// MinHistory is the number of days with any spend the baseline needs.
	// Services with less are "still learning" and are not flagged.
	MinHistory int
	// SameWeekday compares against the same weekday only (when enough samples
	// exist), so weekly patterns such as a Saturday batch job are not flagged.
	SameWeekday bool
	// SigmaFloorUSD is the smallest spread, in USD, ever used. It stops tiny
	// services from flagging on pennies.
	SigmaFloorUSD float64
}

// relativeFloor is the smallest spread, as a fraction of the median, that is
// ever used. It is derived from the two user-facing knobs so a flat series is
// flagged as soon as it rises by MinDeltaPct: the z-score of a rise of
// MinDeltaPct% then equals ZThreshold. A fixed fraction would silently raise
// the effective minimum jump (10% with a 3.5 threshold means 35%), making big,
// steady services insensitive to a $9k/month increase.
func (d Detector) relativeFloor() float64 {
	if d.ZThreshold <= 0 {
		return 0
	}
	return d.MinDeltaPct / 100 / d.ZThreshold
}

// validate rejects settings that would make detection meaningless, so a typo
// fails at startup instead of silently disabling alerts.
func (d Detector) validate(lookbackDays int) error {
	switch {
	case d.ZThreshold <= 0:
		return fmt.Errorf("zscore_threshold must be positive, got %v", d.ZThreshold)
	case d.MinDeltaPct < 0:
		return fmt.Errorf("anomaly_min_delta_pct must not be negative, got %v", d.MinDeltaPct)
	case d.SigmaFloorUSD < 0:
		return fmt.Errorf("sigma_floor_usd must not be negative, got %v", d.SigmaFloorUSD)
	case d.MinHistory < 0:
		return fmt.Errorf("min_history_days must not be negative, got %d", d.MinHistory)
	case lookbackDays < 1:
		return errors.New("lookback_days must be at least 1")
	case d.MinHistory > lookbackDays:
		return fmt.Errorf("min_history_days (%d) cannot exceed lookback_days (%d): no service could ever leave the learning state",
			d.MinHistory, lookbackDays)
	}
	return nil
}

// observation is one service's spend on one day of the baseline window.
type observation struct {
	Day    time.Time
	Amount float64
}

// Verdict is the outcome of assessing one day of spend.
type Verdict struct {
	Anomaly bool
	// Z is the robust z-score (how many MAD-scaled deviations above the median).
	Z float64
	// Learning means there was too little history to judge; Anomaly is false.
	Learning bool
}

// Assess judges value (the spend on day) against history, the service's
// zero-filled daily observations for the baseline window. prev is the previous
// period's spend, used for the minimum-increase check.
func (d Detector) Assess(history []observation, day time.Time, value, prev float64) Verdict {
	if billedDays(history) < d.MinHistory {
		return Verdict{Learning: true}
	}

	series := amounts(history, nil)
	if d.SameWeekday {
		if same := amounts(history, &day); len(same) >= minSameWeekdaySamples {
			series = same
		}
	}

	// A service billed on fewer than half of the days has a median of zero, and
	// every billed day would then look like a spike. Compare billed days with
	// other billed days instead.
	if median(series) == 0 {
		if billed := nonZero(series); len(billed) > 0 {
			series = billed
		}
	}

	med := median(series)
	sigma := math.Max(madScale*mad(series, med), math.Max(d.relativeFloor()*med, math.Max(d.SigmaFloorUSD, minSigmaUSD)))
	z := (value - med) / sigma

	v := Verdict{Z: z}
	if z <= d.ZThreshold {
		return v
	}
	if prev == 0 {
		v.Anomaly = true
		return v
	}
	v.Anomaly = (value-prev)/prev*100 >= d.MinDeltaPct
	return v
}

// billedDays counts the days with any spend.
func billedDays(history []observation) int {
	n := 0
	for _, o := range history {
		if o.Amount > 0 {
			n++
		}
	}
	return n
}

// amounts extracts the spend values, optionally only those on the same weekday as *day.
func amounts(history []observation, day *time.Time) []float64 {
	out := make([]float64, 0, len(history))
	for _, o := range history {
		if day != nil && o.Day.Weekday() != day.Weekday() {
			continue
		}
		out = append(out, o.Amount)
	}
	return out
}

func nonZero(values []float64) []float64 {
	var out []float64
	for _, v := range values {
		if v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// mad is the median absolute deviation of values around med.
func mad(values []float64, med float64) float64 {
	dev := make([]float64, len(values))
	for i, v := range values {
		dev[i] = math.Abs(v - med)
	}
	return median(dev)
}
