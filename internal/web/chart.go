package web

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/prem0x01/costblame/pkg/models"
)

// sparklineWidth/Height/Pad are the fixed viewBox dimensions for the inline
// cost-history chart rendered on the blame detail page.
const (
	sparklineWidth  = 320.0
	sparklineHeight = 64.0
	sparklinePad    = 4.0
)

// sparkline renders an inline animated SVG line chart from real cost
// snapshot history (as returned by store.CostSnapshotsByService). It
// normalizes AmountUSD across the series' actual min/max — nothing here is
// placeholder or synthetic data, only the same rows the store returns.
func sparkline(snaps []models.CostSnapshot) template.HTML {
	if len(snaps) < 2 {
		return template.HTML(`<p class="muted">Not enough history to chart yet.</p>`)
	}

	minV, maxV := snaps[0].AmountUSD, snaps[0].AmountUSD
	for _, s := range snaps {
		if s.AmountUSD < minV {
			minV = s.AmountUSD
		}
		if s.AmountUSD > maxV {
			maxV = s.AmountUSD
		}
	}
	rng := maxV - minV
	if rng == 0 {
		rng = 1
	}

	const innerW = sparklineWidth - 2*sparklinePad
	const innerH = sparklineHeight - 2*sparklinePad
	step := innerW / float64(len(snaps)-1)

	pts := make([]string, len(snaps))
	var lastX, lastY float64
	for i, s := range snaps {
		x := sparklinePad + step*float64(i)
		y := sparklinePad + innerH - ((s.AmountUSD-minV)/rng)*innerH
		pts[i] = fmt.Sprintf("%.2f,%.2f", x, y)
		lastX, lastY = x, y
	}

	svg := fmt.Sprintf(`<svg class="sparkline" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="cost history for this service">
  <polyline class="sparkline-line" points="%s"></polyline>
  <circle class="sparkline-dot" cx="%.2f" cy="%.2f" r="3.2"></circle>
</svg>`, float64(sparklineWidth), float64(sparklineHeight), strings.Join(pts, " "), lastX, lastY)

	return template.HTML(svg)
}
