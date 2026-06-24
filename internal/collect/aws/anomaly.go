package aws

import (
	"math"
)

// baseline holds a rolling window of daily cost amounts used to compute
// the mean and standard deviation for anomaly detection.
type baseline struct {
	values []float64
}

func newBaseline(values []float64) *baseline {
	return &baseline{values: values}
}

func (b *baseline) mean() float64 {
	if len(b.values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range b.values {
		sum += v
	}
	return sum / float64(len(b.values))
}

func (b *baseline) stddev() float64 {
	if len(b.values) < 2 {
		return 0
	}
	m := b.mean()
	var variance float64
	for _, v := range b.values {
		diff := v - m
		variance += diff * diff
	}
	return math.Sqrt(variance / float64(len(b.values)-1))
}

// zScore returns how many standard deviations `value` is above the baseline mean.
// A negative z-score means the value is below the mean (cost decrease).
func (b *baseline) zScore(value float64) float64 {
	sd := b.stddev()
	if sd == 0 {
		return 0
	}
	return (value - b.mean()) / sd
}

// isAnomaly returns true when the z-score exceeds the threshold AND the absolute
// delta exceeds the minimum percentage increase. This avoids flagging tiny services
// with naturally noisy spend as anomalous.
func (b *baseline) isAnomaly(value, prevValue, minDeltaPct, zThreshold float64) (bool, float64) {
	z := b.zScore(value)
	if z <= zThreshold {
		return false, z
	}
	if prevValue == 0 {
		return true, z
	}
	deltaPct := ((value - prevValue) / prevValue) * 100
	return deltaPct >= minDeltaPct, z
}
