-- 0004_add_agent_traces_outcome_columns.sql (PostgreSQL)
--
-- Promotes the low-cardinality derived attribution axes out of the trace_json blob
-- into queryable columns on agent_traces, so the production observability dashboard
-- can GROUP BY them directly instead of re-inferring intent/outcome from reply text.
-- High-cardinality values (selected_instance_id, fact_cache_oldest_age) deliberately
-- stay inside trace_json.
--
-- Deploy order (see deploy/migrations/README.md):
--   1. Apply this migration FIRST (after 0002 created agent_traces).
--   2. Roll out the new compshare-agent binary AFTER.
--
-- Unlike sessions (0003), agent_traces is NOT column-gated by the boot VerifySchema
-- (VerifyTraceSchema probes only that the table exists). The trace writer SOFT-probes
-- for these columns at startup (detectPromotedColumns via information_schema) and
-- degrades to the legacy 12-column INSERT — writing only trace_json — when absent. So
-- a new binary on an un-migrated DB still INGESTS traces (just without the GROUP-BY
-- columns), and an old binary on the new schema is unaffected (columns stay NULL).
--
-- All columns are NULLable: an axis that did not fire this turn is NULL (e.g. a clean
-- answer has refusal_type = NULL), so COUNT(refusal_type) counts only refusals.
-- (PostgreSQL appends columns at the end of the row; MySQL's `AFTER` clauses have no
-- equivalent and are unnecessary.)

ALTER TABLE agent_traces
  ADD COLUMN IF NOT EXISTS terminated_by     VARCHAR(32),
  ADD COLUMN IF NOT EXISTS abort_cause       VARCHAR(32),
  ADD COLUMN IF NOT EXISTS error_class       VARCHAR(32),
  ADD COLUMN IF NOT EXISTS resolution        VARCHAR(32),
  ADD COLUMN IF NOT EXISTS route_status      VARCHAR(48),
  ADD COLUMN IF NOT EXISTS refusal_type      VARCHAR(32),
  ADD COLUMN IF NOT EXISTS resolution_source VARCHAR(32);

CREATE INDEX IF NOT EXISTS idx_terminated_time   ON agent_traces (terminated_by, created_at);
CREATE INDEX IF NOT EXISTS idx_refusal_time      ON agent_traces (refusal_type, created_at);
CREATE INDEX IF NOT EXISTS idx_route_status_time ON agent_traces (route_status, created_at);
