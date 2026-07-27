-- 0011_create_ssh_ops_audit.sql (PostgreSQL)
--
-- Fail-closed audit for the consent-gated, read-only in-instance SSH-ops lane
-- (COMPSHARE_SSH_OPS). One row per consented diagnosis attempt: Begin inserts the
-- 'started' row BEFORE the harness runs (no run without a durable record), Finish
-- enriches it with the outcome. The SSH credential is NEVER written — only tenant
-- identity, the target instance, the PII-redacted task text, and the outcome.
--
-- INV-9 (replay dedup): UNIQUE (turn_id, task_hash) makes a durable replay of the
-- SAME turn collide on Begin, which the fail-closed writer turns into a refusal, so
-- a recovered/retried turn cannot re-enter the box. turn_id is always non-empty on
-- the durable/server path (it is the turn identity); task_hash is the sha256 of the
-- raw task. The engine-side per-turn gate (INV-11) covers the same-turn one-word
-- Task tweak; this key covers cross-turn/replay only.

CREATE TABLE IF NOT EXISTS ssh_ops_audit (
    id                  VARCHAR(64)  PRIMARY KEY,
    request_uuid        VARCHAR(64)  NOT NULL,
    turn_id             VARCHAR(64)  NOT NULL,
    task_hash           VARCHAR(64)  NOT NULL,
    top_organization_id BIGINT       NOT NULL,
    organization_id     BIGINT       NOT NULL,
    instance_id         VARCHAR(64)  NOT NULL,
    task                TEXT         NOT NULL,
    phase               VARCHAR(16)  NOT NULL,
    disposition         VARCHAR(16)  NOT NULL,
    exit_code           INT,
    timed_out           BOOLEAN,
    output_bytes        INT,
    err_class           VARCHAR(64),
    started_at          TIMESTAMPTZ  NOT NULL,
    finished_at         TIMESTAMPTZ,
    CONSTRAINT uq_ssh_ops_turn_task UNIQUE (turn_id, task_hash),
    CONSTRAINT ck_ssh_ops_disposition CHECK (disposition IN ('started', 'ok', 'error'))
);
CREATE INDEX idx_ssh_ops_org_time ON ssh_ops_audit (top_organization_id, organization_id, started_at);
CREATE INDEX idx_ssh_ops_instance ON ssh_ops_audit (instance_id, started_at);
