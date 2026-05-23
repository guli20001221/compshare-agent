-- 0003_add_session_context_version.sql
--
-- Adds context_version column to sessions table for optimistic concurrency
-- control on the agent SessionState envelope stored in sessions.context.
--
-- Deploy order is mandatory (see deploy/migrations/README.md):
--   1. Apply this migration FIRST.
--   2. Roll out the new compshare-agent binary AFTER the migration is applied.
--
-- A new binary started against a database that has not been migrated will
-- fail VerifySchema at boot — sessions.context_version is probed at the
-- column level, not just the table level.
--
-- Old binaries are compatible with the new schema (they don't read this
-- column). Default 0 keeps existing rows readable.

ALTER TABLE sessions
  ADD COLUMN context_version INT NOT NULL DEFAULT 0
  AFTER context;
