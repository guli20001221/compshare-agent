-- 0001_init.sql (PostgreSQL)
--
-- Core chat persistence: sessions / messages / message_feedback.
-- Backend migrated from MySQL/TiDB to PostgreSQL.

CREATE TABLE sessions (
  id                   CHAR(36)     NOT NULL PRIMARY KEY,
  top_organization_id  BIGINT       NOT NULL,
  organization_id      BIGINT       NOT NULL,
  title                VARCHAR(255),
  context              JSONB,
  message_count        INT          NOT NULL DEFAULT 0,
  pinned               BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at           TIMESTAMPTZ
);
CREATE INDEX idx_owner_updated ON sessions (top_organization_id, organization_id, updated_at);

-- Emulates MySQL's `ON UPDATE CURRENT_TIMESTAMP(3)`: bump updated_at on every row
-- UPDATE so ListByOwner recency stays correct even for UPDATEs that don't set it
-- explicitly (UpdateContext, SetTitleIfEmpty). BumpUpdatedAtAndIncCount also sets it
-- explicitly; the trigger is idempotent with that.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_sessions_updated_at
  BEFORE UPDATE ON sessions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE messages (
  id            CHAR(36)     NOT NULL PRIMARY KEY,
  session_id    CHAR(36)     NOT NULL,
  request_uuid  VARCHAR(64),
  role          VARCHAR(16)  NOT NULL,
  content       TEXT         NOT NULL,
  status        VARCHAR(16)  NOT NULL,
  error_code    VARCHAR(64),
  model         VARCHAR(64),
  input_tokens  INT,
  output_tokens INT,
  ttft_ms       INT,
  latency_ms    INT,
  metadata      JSONB,
  created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX idx_session_created ON messages (session_id, created_at);
CREATE INDEX idx_request_uuid ON messages (request_uuid);

CREATE TABLE message_feedback (
  id          CHAR(36)    NOT NULL PRIMARY KEY,
  message_id  CHAR(36)    NOT NULL,
  rating      VARCHAR(8)  NOT NULL,
  comment     TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_message ON message_feedback (message_id);
