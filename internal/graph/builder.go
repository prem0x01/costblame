// Package graph assembles BlameEdge records into a BlameGraph summary.
package graph

import (
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// Build assembles a BlameGraph from a slice of edges all sharing the same
// anomalous cost snapshot. Edges are sorted descending by ConfidenceScore;
// the highest-scoring edge is set as TopBlame.
func Build(anomaly models.CostSnapshot, edges []models.BlameEdge) models.BlameGraph {
	sorted := make([]models.BlameEdge, len(edges))
	copy(sorted, edges)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ConfidenceScore > sorted[j].ConfidenceScore
	})

	g := models.BlameGraph{
		ID:          uuid.New(),
		Anomaly:     anomaly,
		Edges:       sorted,
		GeneratedAt: time.Now().UTC(),
	}
	if len(sorted) > 0 {
		top := sorted[0]
		g.TopBlame = &top
	}
	return g
}
