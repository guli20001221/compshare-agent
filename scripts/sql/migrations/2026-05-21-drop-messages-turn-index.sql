-- Migration: drop agent_messages.turn_index + rebuild idx_connection on created_at.
--
-- Apply once on environments that ran init.sql before 2026-05-21. New
-- deployments get the right schema from init.sql directly.
--
-- Why: agent processes turns serially within one WS connection, so
-- created_at gives strictly-monotonic ordering for any single
-- connection_id. The turn_index column was redundant; created_at +
-- ROW_NUMBER() OVER (PARTITION BY connection_id ORDER BY created_at)
-- gives the same "this is your N-th turn" answer on read.
--
-- agent_traces.turn_index is NOT touched: it's a promoted-column copy
-- of TraceRecord.TurnIndex (observability schema, pinned by plan §15.2).
--
-- Run:
--   docker exec -i agent-mysql mysql -uroot -p<pw> compshare_agent < \
--     scripts/sql/migrations/2026-05-21-drop-messages-turn-index.sql
--
-- WARNING: NOT idempotent. DROP INDEX / DROP COLUMN error on re-run. Track
-- applied migrations in your deploy tooling (or check the schema before
-- running):
--   SHOW COLUMNS FROM agent_messages LIKE 'turn_index';
--   -- empty result = already migrated, skip.

-- The composite index lives on (top_org, conn, turn_index); we drop it
-- before the column so MySQL doesn't complain about leftover index
-- referencing the dropped column.
ALTER TABLE agent_messages
    DROP INDEX idx_connection,
    DROP COLUMN turn_index,
    ADD INDEX idx_connection (top_organization_id, connection_id, created_at);
