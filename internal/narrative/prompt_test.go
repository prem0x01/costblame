package narrative

import (
	"testing"
	"time"
)

func TestDeployTiming(t *testing.T) {
	start := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		deploy time.Time
		want   string
	}{
		{"before period", start.Add(-6 * time.Hour), "6.0 hours before the cost period began"},
		{"during period", start.Add(3 * time.Hour), "3.0 hours into the cost period"},
	}
	for _, tc := range cases {
		if got := deployTiming(start, tc.deploy); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
