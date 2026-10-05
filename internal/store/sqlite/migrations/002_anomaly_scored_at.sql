-- Track when the correlation engine last scored an anomaly.
-- Before this column, "needs scoring" was inferred from the absence of blame
-- edges, so anomalies with no qualifying deploy candidates were re-scored on
-- every cycle forever.

ALTER TABLE cost_snapshots ADD COLUMN scored_at DATETIME;

-- Backfill: anomalies that already have blame edges were evidently scored.
UPDATE cost_snapshots
SET scored_at = (
    SELECT MIN(be.created_at) FROM blame_edges be
    WHERE be.cost_snapshot_id = cost_snapshots.id
)
WHERE id IN (SELECT DISTINCT cost_snapshot_id FROM blame_edges);

CREATE INDEX IF NOT EXISTS idx_cost_snapshots_unscored
    ON cost_snapshots (is_anomaly, scored_at);
