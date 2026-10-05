-- Make blame edges idempotent and track alert delivery.
--
-- Before this migration blame_edges had no uniqueness on (anomaly, deploy) and
-- edge IDs were random, so a retry after a partial failure inserted a second
-- set of edges. Alert delivery was fire-and-forget, so a notifier outage lost
-- the alert permanently.
-- (The migration runner applies each file inside a transaction.)

-- 1. Merge existing duplicates. Keep one edge per (anomaly, deploy), preferring
--    one a human already reviewed (confirmed, then dismissed), then the one
--    already alerted, then the oldest.
DELETE FROM blame_edges
WHERE id IN (
    SELECT id FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                   PARTITION BY cost_snapshot_id, deploy_event_id
                   ORDER BY CASE status
                                WHEN 'confirmed' THEN 0
                                WHEN 'dismissed' THEN 1
                                WHEN 'resolved'  THEN 2
                                ELSE 3
                            END,
                            created_at ASC,
                            id ASC
               ) AS rn
        FROM blame_edges
    )
    WHERE rn > 1
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_blame_edges_pair
    ON blame_edges (cost_snapshot_id, deploy_event_id);

-- 2. Alert outbox marker: NULL means "not delivered yet". Edges that existed
--    before this migration were alerted by the old code path (or never will
--    be); backfill them so an upgrade does not re-send old alerts.
ALTER TABLE blame_edges ADD COLUMN alerted_at DATETIME;

UPDATE blame_edges
SET alerted_at = created_at
WHERE status IN ('resolved', 'confirmed', 'dismissed');

CREATE INDEX IF NOT EXISTS idx_blame_edges_unalerted
    ON blame_edges (status, alerted_at);
