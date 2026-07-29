-- 0006_create_turn_protocol.sql (PostgreSQL)
--
-- Replayable semantic turn events and durable confirmation/selection
-- interactions. These rows are shared by all replicas.

CREATE TABLE IF NOT EXISTS chat_turn_events (
  turn_id      CHAR(36)      NOT NULL,
  seq          BIGINT        NOT NULL,
  lease_epoch  BIGINT        NOT NULL,
  event_type   VARCHAR(64)   NOT NULL,
  payload      JSONB,
  provisional  BOOLEAN      NOT NULL DEFAULT TRUE,
  created_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, seq)
);

CREATE TABLE IF NOT EXISTS turn_interactions (
  id                CHAR(36)      NOT NULL PRIMARY KEY,
  turn_id           CHAR(36)      NOT NULL,
  interaction_key   VARCHAR(128)  NOT NULL,
  kind              VARCHAR(32)   NOT NULL,
  request_hash      CHAR(64)      NOT NULL,
  request_payload   JSONB,
  lease_epoch       BIGINT        NOT NULL,
  expires_at        TIMESTAMPTZ   NOT NULL,
  status            VARCHAR(16)   NOT NULL,
  resolution_hash   CHAR(64),
  response_payload  JSONB,
  created_at        TIMESTAMPTZ   NOT NULL DEFAULT now(),
  resolved_at       TIMESTAMPTZ,
  UNIQUE (turn_id, interaction_key),
  CONSTRAINT ck_turn_interaction_status CHECK (
    (status = 'pending' AND resolution_hash IS NULL AND response_payload IS NULL AND resolved_at IS NULL) OR
    (status = 'resolved' AND resolution_hash IS NOT NULL AND response_payload IS NOT NULL AND resolved_at IS NOT NULL)
  )
);
CREATE INDEX IF NOT EXISTS idx_turn_interactions_turn ON turn_interactions (turn_id, created_at);
