-- 0013_add_ssh_ops_context_observability.sql (PostgreSQL)
--
-- Adds aggregate-only observability for the versioned contextual SSH-ops
-- prompt. No user reports, platform facts or raw commands are persisted here:
-- the columns say only which schema/fact categories reached the harness and
-- what kind of first command it made.
--
-- Reading context_schema_version / context_fact_coverage: only a row with
-- finished_at IS NOT NULL states what was APPLIED. On the 'started' row these
-- columns are what the server REQUESTED, written before the harness exists;
-- Finish zeroes them when the harness did not confirm the context reached the
-- model. A row that never finished keeps the requested value forever, so a
-- query that ignores finished_at over-reports coverage.
--
-- context_fact_coverage is a bitmask defined in internal/opscontext/context.go.
-- Bits are appended, never reordered, but their MEANING is still version-scoped:
-- bit 16 covered the Describe ports block AND the TCP forwards under schema
-- version 1, and only the ports block under version 2, which split them into
-- separate facts and bits. Group by context_schema_version before comparing it.

ALTER TABLE ssh_ops_audit
    ADD COLUMN IF NOT EXISTS context_schema_version INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS context_fact_coverage BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS commands_ran INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS commands_refused INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS first_command_class VARCHAR(64) NOT NULL DEFAULT 'none';
