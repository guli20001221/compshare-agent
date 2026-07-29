-- 0010_add_action_abandonment.sql (PostgreSQL)
--
-- Reservations that never crossed StartAction have no external effect. A
-- later fenced executor may retire them and plan again without pretending the
-- non-deterministic model must reproduce the old ordinal action sequence.

ALTER TABLE turn_actions
  DROP CONSTRAINT IF EXISTS ck_turn_action_status;

ALTER TABLE turn_actions
  ADD CONSTRAINT ck_turn_action_status CHECK (status IN (
    'reserved', 'succeeded', 'failed', 'ambiguous', 'abandoned'
  ));
