-- compshare-agent console-deployment schema (PR3 / A4).
--
-- Apply with:
--   docker exec -i agent-mysql mysql -uroot -p<pw> compshare_agent < scripts/sql/init.sql
--
-- Idempotent: uses IF NOT EXISTS for tables. Schema changes after first
-- deploy must use ALTER TABLE ... ADD COLUMN (MySQL 8.0+ INSTANT DDL is
-- preferred to avoid blocking writes).
--
-- Charset: utf8mb4 is REQUIRED to round-trip emoji and Chinese in
-- user_message / assistant_message / trace_json. Connections must include
-- charset=utf8mb4 in the DSN.

SET NAMES utf8mb4;

-- agent_messages — one row per (user_message, assistant_reply) turn.
-- Sized for full Chinese conversation traces; MEDIUMTEXT holds up to 16MiB.
--
-- No turn_index column: the agent processes turns serially within one
-- WS connection (each Engine.Chat is synchronous), so created_at gives
-- strictly-monotonic ordering for any single connection_id. Operators
-- needing a 1-based turn number can compute
--   ROW_NUMBER() OVER (PARTITION BY connection_id ORDER BY created_at)
-- on read. agent_traces.turn_index stays because TraceRecord schema is
-- pinned (plan §15.2).
--
-- Index choices:
--   uk_request_uuid: dedupes server retries (request_uuid is the engine's
--     per-turn idempotency key).
--   idx_org_time: console "show me my chat history" queries.
--   idx_connection: same-session turn ordering via created_at.
--   idx_created: ops cleanup ("delete rows older than N days").
CREATE TABLE IF NOT EXISTS agent_messages (
    id                  BIGINT       AUTO_INCREMENT PRIMARY KEY,
    request_uuid        VARCHAR(64)  NOT NULL,
    top_organization_id BIGINT       NOT NULL,
    organization_id     BIGINT       NOT NULL,
    connection_id       VARCHAR(64)  NOT NULL,
    created_at          DATETIME(3)  NOT NULL,
    user_message        MEDIUMTEXT,
    assistant_message   MEDIUMTEXT,
    status              ENUM('success','blocked','error') NOT NULL,
    model               VARCHAR(64),
    latency_ms          INT,
    UNIQUE KEY uk_request_uuid (request_uuid),
    KEY idx_org_time (top_organization_id, organization_id, created_at),
    KEY idx_connection (top_organization_id, connection_id, created_at),
    KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- agent_traces — one row per turn carrying the full TraceRecord JSON +
-- the most-queried scalar projections (intent, status, tool_count, etc.)
-- so dashboards don't need to crack the JSON for common filters.
--
-- cited_chunk_ids is a JSON array of strings; emit [] for "no citation"
-- (not null) so consumers can iterate uniformly. Writer enforces this.
CREATE TABLE IF NOT EXISTS agent_traces (
    id                  BIGINT       AUTO_INCREMENT PRIMARY KEY,
    request_uuid        VARCHAR(64)  NOT NULL,
    top_organization_id BIGINT       NOT NULL,
    organization_id     BIGINT       NOT NULL,
    connection_id       VARCHAR(64)  NOT NULL,
    turn_index          INT          NOT NULL,
    created_at          DATETIME(3)  NOT NULL,
    status              ENUM('success','blocked','error') NOT NULL,
    intent              VARCHAR(32),
    tool_count          INT,
    cited_chunk_ids     JSON         NOT NULL,
    duration_ms         INT,
    trace_json          JSON         NOT NULL,
    UNIQUE KEY uk_request_uuid (request_uuid),
    KEY idx_org_time (top_organization_id, organization_id, created_at),
    KEY idx_status_time (status, created_at),
    KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
