CREATE TABLE sessions (
  id                   CHAR(36)     NOT NULL PRIMARY KEY,
  top_organization_id  INT UNSIGNED NOT NULL,
  organization_id      INT UNSIGNED NOT NULL,
  title                VARCHAR(255) NULL,
  context              JSON         NULL,
  message_count        INT          NOT NULL DEFAULT 0,
  pinned               TINYINT(1)   NOT NULL DEFAULT 0,
  created_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at           DATETIME(3)  NULL,
  KEY idx_owner_updated (top_organization_id, organization_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE messages (
  id            CHAR(36)     NOT NULL PRIMARY KEY,
  session_id    CHAR(36)     NOT NULL,
  request_uuid  VARCHAR(64)  NULL,
  role          VARCHAR(16)  NOT NULL,
  content       MEDIUMTEXT   NOT NULL,
  status        VARCHAR(16)  NOT NULL,
  error_code    VARCHAR(64)  NULL,
  model         VARCHAR(64)  NULL,
  input_tokens  INT          NULL,
  output_tokens INT          NULL,
  ttft_ms       INT          NULL,
  latency_ms    INT          NULL,
  metadata      JSON         NULL,
  created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_session_created (session_id, created_at),
  KEY idx_request_uuid    (request_uuid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE message_feedback (
  id          CHAR(36)    NOT NULL PRIMARY KEY,
  message_id  CHAR(36)    NOT NULL,
  rating      VARCHAR(8)  NOT NULL,
  comment     TEXT        NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_message (message_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
