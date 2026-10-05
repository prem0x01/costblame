package aws

import (
	"math"
	"strings"
	"testing"
	"time"
)

var monday = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // a Monday

func testDetector() Detector {
	return Detector{ZThreshold: 3.5, MinDeltaPct: 20, MinHistory: 7, SigmaFloorUSD: 1}
}

// series builds consecutive daily observations starting at start.
func series(start time.Time, vals ...float64) []observation {
	out := make([]observation, len(vals))
	for i, v := range vals {
		out[i] = observation{Day: start.AddDate(0, 0, i), Amount: v}
	}
	return out
}

func repeat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestMedianAndMAD(t *testing.T) {
	cases := []struct {
		vals            []float64
		wantMed, wantMA float64
	}{
		{nil, 0, 0},
		{[]float64{5}, 5, 0},
		{[]float64{1, 2, 3}, 2, 1},
		{[]float64{1, 2, 3, 4}, 2.5, 1},
		{[]float64{5, 5, 5, 5, 200}, 5, 0}, // one outlier does not move either
	}
	for _, tc := range cases {
		med := median(tc.vals)
		if med != tc.wantMed {
			t.Errorf("median(%v) = %v, want %v", tc.vals, med, tc.wantMed)
		}
		if got := mad(tc.vals, med); got != tc.wantMA {
			t.Errorf("mad(%v) = %v, want %v", tc.vals, got, tc.wantMA)
		}
	}
}

func TestAssess(t *testing.T) {
	day := monday.AddDate(0, 0, 30) // the day being judged

	flat := series(monday, repeat(5, 30)...)

	// 29 quiet days and one big spike 10 days ago.
	masked := series(monday, repeat(5, 30)...)
	masked[20].Amount = 200

	// Billed 8 of 30 days, $10 each time: an intermittent job.
	sparse := make([]float64, 30)
	for _, i := range []int{1, 5, 9, 12, 16, 20, 24, 28} {
		sparse[i] = 10
	}

	cases := []struct {
		name         string
		d            Detector
		history      []observation
		value, prev  float64
		wantAnomaly  bool
		wantLearning bool
	}{
		{"flat $5 then $500 is flagged (stddev would be 0)", testDetector(), flat, 500, 5, true, false},
		{"flat and unchanged is not flagged", testDetector(), flat, 5, 5, false, false},
		{"small wobble is not flagged", testDetector(), series(monday, alternate(5, 6, 30)...), 7, 6, false, false},
		{"a past spike does not mask a later one", testDetector(), masked, 60, 5, true, false},
		{"a cost decrease is never an anomaly", testDetector(), series(monday, repeat(100, 30)...), 1, 100, false, false},
		{"pennies on a tiny service do not flag", testDetector(), series(monday, repeat(0.02, 30)...), 0.5, 0.02, false, false},
		{"large jump but under the minimum increase", testDetector(), flat, 20, 18, false, false},
		{"spike after a zero day is flagged", testDetector(), flat, 500, 0, true, false},

		{"too little history: still learning, never flagged", testDetector(), series(monday, append(repeat(0, 27), 5, 5, 5)...), 500, 5, false, true},
		{"exactly the minimum history can flag", testDetector(), series(monday, append(repeat(0, 23), repeat(5, 7)...)...), 500, 5, true, false},
		{"MinHistory 0 disables learning", Detector{ZThreshold: 3.5, MinDeltaPct: 20, SigmaFloorUSD: 1}, series(monday, append(repeat(0, 29), 5)...), 500, 5, true, false},

		{"intermittent service: a normal run is not flagged", testDetector(), series(monday, sparse...), 10, 0, false, false},
		{"intermittent service: a 10x run is flagged", testDetector(), series(monday, sparse...), 100, 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.d.Assess(tc.history, day, tc.value, tc.prev)
			if v.Anomaly != tc.wantAnomaly || v.Learning != tc.wantLearning {
				t.Errorf("Anomaly=%v Learning=%v (z=%.1f), want Anomaly=%v Learning=%v",
					v.Anomaly, v.Learning, v.Z, tc.wantAnomaly, tc.wantLearning)
			}
			if math.IsInf(v.Z, 0) || math.IsNaN(v.Z) {
				t.Errorf("z-score %v is not finite; it is stored and serialized as JSON", v.Z)
			}
		})
	}
}

func alternate(a, b float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = a
		} else {
			out[i] = b
		}
	}
	return out
}

// A weekly pattern: $10 on weekdays, $100 on Saturdays.
func weekly(days int) []observation {
	out := make([]observation, days)
	for i := range out {
		d := monday.AddDate(0, 0, i)
		amt := 10.0
		if d.Weekday() == time.Saturday {
			amt = 100
		}
		out[i] = observation{Day: d, Amount: amt}
	}
	return out
}

func TestAssess_SameWeekday(t *testing.T) {
	saturday := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	if saturday.Weekday() != time.Saturday {
		t.Fatal("test date is not a Saturday")
	}
	history := weekly(35) // five weeks, five Saturdays

	all := testDetector()
	if v := all.Assess(history, saturday, 100, 10); !v.Anomaly {
		t.Fatalf("without weekday awareness a normal Saturday is expected to look anomalous (z=%.1f); this documents why the option exists", v.Z)
	}

	aware := testDetector()
	aware.SameWeekday = true
	if v := aware.Assess(history, saturday, 100, 10); v.Anomaly {
		t.Errorf("normal Saturday flagged despite SameWeekday (z=%.1f)", v.Z)
	}
	if v := aware.Assess(history, saturday, 1000, 10); !v.Anomaly {
		t.Errorf("a 10x Saturday should still be flagged (z=%.1f)", v.Z)
	}
}

func TestAssess_SameWeekdayFallsBackWithoutEnoughSamples(t *testing.T) {
	// Ten days contain at most two of any weekday, under minSameWeekdaySamples.
	history := series(monday, repeat(5, 10)...)
	d := testDetector()
	d.SameWeekday = true
	d.MinHistory = 7

	if v := d.Assess(history, monday.AddDate(0, 0, 10), 500, 5); !v.Anomaly || v.Learning {
		t.Errorf("should fall back to the all-days baseline and flag, got %+v", v)
	}
}

func TestDetectorValidate(t *testing.T) {
	ok := testDetector()
	cases := []struct {
		name     string
		mod      func(*Detector)
		lookback int
		wantErr  string
	}{
		{"valid", func(*Detector) {}, 30, ""},
		{"zero threshold", func(d *Detector) { d.ZThreshold = 0 }, 30, "zscore_threshold"},
		{"negative delta", func(d *Detector) { d.MinDeltaPct = -1 }, 30, "anomaly_min_delta_pct"},
		{"negative floor", func(d *Detector) { d.SigmaFloorUSD = -1 }, 30, "sigma_floor_usd"},
		{"negative history", func(d *Detector) { d.MinHistory = -1 }, 30, "min_history_days"},
		{"history longer than lookback can never be met", func(d *Detector) { d.MinHistory = 40 }, 30, "cannot exceed lookback_days"},
		{"zero lookback", func(*Detector) {}, 0, "lookback_days"},
	}
	for _, tc := range cases {
		d := ok
		tc.mod(&d)
		err := d.validate(tc.lookback)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.wantErr)
		}
	}
}

// A big, steady service: a spike must be caught once it passes the minimum
// delta, not only after a much larger jump. With a fixed 10% relative floor and
// a 3.5 threshold this needed +35%, so +25% on $1,000/day (about $7k a month)
// went unnoticed.
func TestAssess_BigSteadyServiceIsSensitiveToTheMinimumDelta(t *testing.T) {
	day := monday.AddDate(0, 0, 30)
	noisy := series(monday, alternate(995, 1005, 30)...) // ~$1,000/day with 1% noise
	d := testDetector()                                  // z 3.5, min delta 20%

	cases := []struct {
		value       float64
		wantAnomaly bool
		why         string
	}{
		{1300, true, "+30%"},
		{1250, true, "+25%: above the 20% minimum delta, previously missed"},
		{1215, true, "just above the minimum delta"},
		{1150, false, "+15% is below the 20% minimum delta"},
		{1005, false, "ordinary noise"},
		{500, false, "a decrease is never an anomaly"},
	}
	for _, tc := range cases {
		v := d.Assess(noisy, day, tc.value, 1000)
		if v.Anomaly != tc.wantAnomaly {
			t.Errorf("$%.0f (%s): Anomaly=%v (z=%.2f), want %v", tc.value, tc.why, v.Anomaly, v.Z, tc.wantAnomaly)
		}
	}
}

func TestRelativeFloor_DerivedFromTheTwoKnobs(t *testing.T) {
	cases := []struct {
		z, minDelta, want float64
	}{
		{3.5, 20, 20.0 / 100 / 3.5},
		{2, 50, 0.25},
		{3.5, 0, 0}, // no minimum delta: no relative floor
		{0, 20, 0},  // invalid threshold must not divide by zero
	}
	for _, tc := range cases {
		got := Detector{ZThreshold: tc.z, MinDeltaPct: tc.minDelta}.relativeFloor()
		if math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("relativeFloor(z=%v, minDelta=%v) = %v, want %v", tc.z, tc.minDelta, got, tc.want)
		}
	}
}
