-- 0004_add_agent_traces_outcome_columns.sql
--
-- Promotes the low-cardinality derived attribution axes out of the trace_json blob
-- into queryable columns on agent_traces, so the production observability dashboard
-- can GROUP BY them directly instead of re-inferring intent/outcome from reply text
-- (the "其他 = 28%" offline artifact). High-cardinality values
-- (selected_instance_id, fact_cache_oldest_age) deliberately stay inside trace_json.
--
-- Deploy order (see deploy/migrations/README.md):
--   1. Apply this migration FIRST (after 0002 created agent_traces).
--   2. Roll out the new compshare-agent binary AFTER.
--
-- Unlike sessions (0003), agent_traces is NOT column-gated by the boot VerifySchema
-- (VerifyTraceSchema probes only that the table exists). The MySQL trace writer
-- SOFT-probes for these columns at startup (detectPromotedColumns) and degrades to
-- the legacy 12-column INSERT — writing only trace_json — when they are absent. So:
--   * a new binary on an un-migrated DB still INGESTS traces (just without the
--     GROUP-BY columns), and
--   * an OLD binary on the new schema is unaffected (it never references these
--     columns; they stay NULL).
-- This makes the migration order forgiving for trace ingestion specifically; the
-- columns are a queryability optimization, never a correctness gate.
--
-- All columns are NULLable: an axis that did not fire this turn is NULL (e.g. a
-- clean answer has refusal_type = NULL, error_class = NULL), so COUNT(refusal_type)
-- counts only refusals and GROUP BY buckets stay meaningful.

ALTER TABLE agent_traces
  ADD COLUMN terminated_by     VARCHAR(32) NULL AFTER status,
  ADD COLUMN abort_cause       VARCHAR(32) NULL AFTER terminated_by,
  ADD COLUMN error_class       VARCHAR(32) NULL AFTER abort_cause,
  ADD COLUMN resolution        VARCHAR(32) NULL AFTER error_class,
  ADD COLUMN route_status      VARCHAR(48) NULL AFTER resolution,
  ADD COLUMN refusal_type      VARCHAR(32) NULL AFTER route_status,
  ADD COLUMN resolution_source VARCHAR(32) NULL AFTER refusal_type;

CREATE INDEX idx_terminated_time   ON agent_traces (terminated_by, created_at);
CREATE INDEX idx_refusal_time      ON agent_traces (refusal_type, created_at);
CREATE INDEX idx_route_status_time ON agent_traces (route_status, created_at);
