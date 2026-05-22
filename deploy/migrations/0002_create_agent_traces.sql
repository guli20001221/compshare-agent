SET NAMES utf8mb4;

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
