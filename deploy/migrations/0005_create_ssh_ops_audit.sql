-- 0005_create_ssh_ops_audit.sql
--
-- Audit trail for the consent-gated, read-only in-instance SSH diagnosis lane (COMPSHARE_SSH_OPS).
-- One row per consented attempt. The SSH credential is NEVER stored — only tenant identity, the
-- target instance, the task text, and the outcome.
--
-- Apply this BEFORE enabling COMPSHARE_SSH_OPS=1. The Go writer (internal/store/audit.go) is
-- fail-closed: if this table is absent the diagnosis is REFUSED rather than run unlogged. The
-- feature is default-off, so existing deployments that never enable it do not need this migration
-- (the server does not probe for the table at boot).
--
-- PostgreSQL (despite the MYSQL_DSN env var name; the backing store is Postgres). Apply via psql,
-- migration-first then binary-second, per deploy/migrations/README.md.

CREATE TABLE IF NOT EXISTS ssh_ops_audit (
    id                  TEXT PRIMARY KEY,
    request_uuid        TEXT        NOT NULL,
    top_organization_id BIGINT      NOT NULL,
    organization_id     BIGINT      NOT NULL,
    instance_id         TEXT        NOT NULL,
    task                TEXT        NOT NULL DEFAULT '',
    phase               TEXT        NOT NULL DEFAULT 'read_only',
    disposition         TEXT        NOT NULL DEFAULT 'started',  -- started | ok | error
    exit_code           INTEGER,
    timed_out           BOOLEAN     NOT NULL DEFAULT false,
    output_bytes        INTEGER     NOT NULL DEFAULT 0,
    err_class           TEXT        NOT NULL DEFAULT '',
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_ssh_ops_audit_org_started
    ON ssh_ops_audit (top_organization_id, organization_id, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_ssh_ops_audit_instance
    ON ssh_ops_audit (instance_id, started_at DESC);
