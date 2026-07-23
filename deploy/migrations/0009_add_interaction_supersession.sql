-- 0009_add_interaction_supersession.sql (PostgreSQL)
--
-- A confirmation is identified by its semantic payload rather than its ordinal
-- position. When a recovered model proposes a different card, the old pending
-- card is retained for audit but may no longer be resolved.

ALTER TABLE turn_interactions
  DROP CONSTRAINT ck_turn_interaction_status;

ALTER TABLE turn_interactions
  ADD COLUMN interaction_generation BIGSERIAL;

WITH ranked AS (
  SELECT id, ROW_NUMBER() OVER (PARTITION BY turn_id ORDER BY created_at, id) AS generation
  FROM turn_interactions
)
UPDATE turn_interactions AS target
SET interaction_generation = ranked.generation
FROM ranked
WHERE target.id = ranked.id;

ALTER TABLE turn_interactions
  ADD CONSTRAINT ck_turn_interaction_status CHECK (
    (status = 'pending' AND resolution_hash IS NULL AND response_payload IS NULL AND resolved_at IS NULL) OR
    (status = 'resolved' AND resolution_hash IS NOT NULL AND response_payload IS NOT NULL AND resolved_at IS NOT NULL) OR
    (status = 'superseded' AND resolution_hash IS NULL AND response_payload IS NULL AND resolved_at IS NULL)
  );

CREATE INDEX idx_turn_interactions_pending
  ON turn_interactions (turn_id, created_at)
  WHERE status = 'pending';

CREATE UNIQUE INDEX uq_turn_interactions_generation
  ON turn_interactions (turn_id, interaction_generation);
