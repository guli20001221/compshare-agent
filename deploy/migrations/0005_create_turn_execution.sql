-- 0005_create_turn_execution.sql (PostgreSQL)
--
-- Durable turn identity, per-conversation execution fencing, and idempotent
-- external actions. All additions are nullable/new-table only so binaries that
-- predate the turn protocol continue to operate during a migration-first roll.

CREATE TABLE IF NOT EXISTS chat_turns (
  id                         CHAR(36)      NOT NULL PRIMARY KEY,
  session_id                 CHAR(36)      NOT NULL,
  top_organization_id        BIGINT        NOT NULL,
  organization_id            BIGINT        NOT NULL,
  client_turn_id             VARCHAR(128)  NOT NULL,
  turn_seq                   BIGINT        NOT NULL,
  request_hash               CHAR(64)      NOT NULL,
  status                     VARCHAR(32)   NOT NULL,
  user_message_id            CHAR(36)      NOT NULL,
  assistant_message_id       CHAR(36)      NOT NULL,
  base_context_version       INT           NOT NULL,
  committed_context_version  INT,
  committed_lease_epoch      BIGINT,
  commit_hash                CHAR(64),
  error_code                 VARCHAR(64),
  executor_id                VARCHAR(128),
  lease_epoch                BIGINT,
  has_external_action        BOOLEAN       NOT NULL DEFAULT FALSE,
  next_event_seq             BIGINT        NOT NULL DEFAULT 1,
  created_at                 TIMESTAMPTZ   NOT NULL DEFAULT now(),
  updated_at                 TIMESTAMPTZ   NOT NULL DEFAULT now(),
  started_at                 TIMESTAMPTZ,
  finished_at                TIMESTAMPTZ,
  committed_at               TIMESTAMPTZ,
  CONSTRAINT uq_chat_turn_client_id
    UNIQUE (top_organization_id, organization_id, session_id, client_turn_id),
  CONSTRAINT uq_chat_turn_sequence UNIQUE (session_id, turn_seq),
  CONSTRAINT ck_chat_turn_status CHECK (status IN (
    'accepted', 'running', 'awaiting_confirmation', 'committing',
    'committed', 'failed_retryable', 'ambiguous_after_action', 'aborted'
  ))
);
CREATE INDEX IF NOT EXISTS idx_chat_turns_session_created
  ON chat_turns (session_id, turn_seq);

DROP TRIGGER IF EXISTS trg_chat_turns_updated_at ON chat_turns;
CREATE TRIGGER trg_chat_turns_updated_at
  BEFORE UPDATE ON chat_turns
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS conversation_leases (
  session_id                CHAR(36)      NOT NULL PRIMARY KEY,
  top_organization_id       BIGINT        NOT NULL,
  organization_id           BIGINT        NOT NULL,
  active_turn_id            CHAR(36),
  holder_id                 VARCHAR(128),
  lease_epoch               BIGINT        NOT NULL DEFAULT 0,
  lease_until               TIMESTAMPTZ   NOT NULL DEFAULT now(),
  created_at                TIMESTAMPTZ   NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ   NOT NULL DEFAULT now(),
  CONSTRAINT ck_conversation_lease_binding CHECK (
    (active_turn_id IS NULL AND holder_id IS NULL) OR
    (active_turn_id IS NOT NULL AND holder_id IS NOT NULL)
  )
);

DROP TRIGGER IF EXISTS trg_conversation_leases_updated_at ON conversation_leases;
CREATE TRIGGER trg_conversation_leases_updated_at
  BEFORE UPDATE ON conversation_leases
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS turn_actions (
  turn_id          CHAR(36)      NOT NULL,
  action_index     INT           NOT NULL,
  lease_epoch      BIGINT        NOT NULL,
  action_name      VARCHAR(128)  NOT NULL,
  args_hash        CHAR(64)      NOT NULL,
  execution_token  CHAR(36)      NOT NULL,
  in_flight        BOOLEAN       NOT NULL DEFAULT FALSE,
  upstream_request_id VARCHAR(128),
  status            VARCHAR(16)  NOT NULL,
  result             JSONB,
  error_code         VARCHAR(64),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, action_index),
  CONSTRAINT uq_turn_action_execution_token UNIQUE (execution_token),
  CONSTRAINT uq_turn_action_semantic UNIQUE (turn_id, action_name, args_hash),
  CONSTRAINT ck_turn_action_status CHECK (status IN (
    'reserved', 'succeeded', 'failed', 'ambiguous'
  )),
  CONSTRAINT ck_turn_action_in_flight CHECK (
    status = 'reserved' OR NOT in_flight
  )
);

DROP TRIGGER IF EXISTS trg_turn_actions_updated_at ON turn_actions;
CREATE TRIGGER trg_turn_actions_updated_at
  BEFORE UPDATE ON turn_actions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS turn_id CHAR(36),
  ADD COLUMN IF NOT EXISTS turn_role VARCHAR(16);

ALTER TABLE messages
  DROP CONSTRAINT IF EXISTS ck_message_turn_role;

ALTER TABLE messages
  ADD CONSTRAINT ck_message_turn_role CHECK (
    (turn_id IS NULL AND turn_role IS NULL) OR
    (turn_id IS NOT NULL AND turn_role IN ('user', 'assistant'))
  );

CREATE UNIQUE INDEX IF NOT EXISTS uq_messages_turn_role
  ON messages (turn_id, turn_role)
  WHERE turn_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_turn_id ON messages (turn_id);
