-- 0003_add_session_context_version.sql (PostgreSQL)
--
-- Adds context_version column to sessions for optimistic concurrency control on
-- the agent SessionState envelope stored in sessions.context.
--
-- Deploy order is mandatory (see deploy/migrations/README.md):
--   1. Apply this migration FIRST.
--   2. Roll out the new compshare-agent binary AFTER the migration is applied.
--
-- A new binary started against an un-migrated database will fail VerifySchema at
-- boot — sessions.context_version is probed at the column level, not just the
-- table level. Default 0 keeps existing rows readable.
--
-- (PostgreSQL adds columns at the end of the row; MySQL's `AFTER context` clause
-- has no equivalent and is unnecessary.)

ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS context_version INT NOT NULL DEFAULT 0;
