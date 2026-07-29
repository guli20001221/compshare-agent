-- 0002_create_agent_traces.sql (PostgreSQL)
--
-- HTTP per-turn trace persistence (observability.MySQLWriter, backend now
-- PostgreSQL). One row per turn, keyed by request_uuid; the writer uses
-- INSERT ... ON CONFLICT (request_uuid) DO NOTHING so retries are idempotent.

CREATE TABLE IF NOT EXISTS agent_traces (
    id                  BIGSERIAL    PRIMARY KEY,
    request_uuid        VARCHAR(64)  NOT NULL,
    top_organization_id BIGINT       NOT NULL,
    organization_id     BIGINT       NOT NULL,
    connection_id       VARCHAR(64)  NOT NULL,
    turn_index          INT          NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL,
    status              VARCHAR(16)  NOT NULL,
    intent              VARCHAR(32),
    tool_count          INT,
    cited_chunk_ids     JSONB        NOT NULL,
    duration_ms         INT,
    trace_json          JSONB        NOT NULL,
    CONSTRAINT uk_request_uuid UNIQUE (request_uuid),
    CONSTRAINT chk_agent_traces_status CHECK (status IN ('success','blocked','error'))
);
CREATE INDEX IF NOT EXISTS idx_org_time ON agent_traces (top_organization_id, organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_status_time ON agent_traces (status, created_at);
CREATE INDEX IF NOT EXISTS idx_created ON agent_traces (created_at);
