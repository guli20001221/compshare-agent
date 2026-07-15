-- 0008_add_turn_retry_policy.sql (PostgreSQL)
--
-- Recovery retries are durable and database-enforced. A replica restart or a
-- second replica therefore cannot reset the attempt budget or bypass backoff.

ALTER TABLE chat_turns
  ADD COLUMN retry_count INT NOT NULL DEFAULT 0,
  ADD COLUMN next_retry_at TIMESTAMPTZ;

ALTER TABLE chat_turns
  DROP CONSTRAINT ck_chat_turn_status;

ALTER TABLE chat_turns
  ADD CONSTRAINT ck_chat_turn_status CHECK (status IN (
    'accepted', 'running', 'awaiting_confirmation', 'committing',
    'committed', 'failed_retryable', 'failed_final',
    'ambiguous_after_action', 'aborted'
  )),
  ADD CONSTRAINT ck_chat_turn_retry_count CHECK (retry_count BETWEEN 0 AND 3);

-- Rows created by a migration-first deployment before the new binary starts
-- are immediately eligible rather than becoming permanent queue heads.
UPDATE chat_turns
SET retry_count = 1, next_retry_at = NOW()
WHERE status = 'failed_retryable' AND next_retry_at IS NULL;

ALTER TABLE chat_turns
  ADD CONSTRAINT ck_chat_turn_retry_schedule CHECK (
    (status = 'failed_retryable' AND next_retry_at IS NOT NULL)
    OR (status <> 'failed_retryable' AND next_retry_at IS NULL)
  );

DROP INDEX idx_chat_turns_recovery;
CREATE INDEX idx_chat_turns_recovery
  ON chat_turns (status, next_retry_at, updated_at)
  WHERE status IN (
    'accepted', 'running', 'awaiting_confirmation', 'committing',
    'failed_retryable'
  );

-- The application already validates every context hint. Tighten the database
-- boundary too so a direct writer cannot smuggle objects into resource_ids and
-- later have them projected into an agent prompt.
ALTER TABLE turn_actions
  DROP CONSTRAINT ck_turn_action_context_hint;

ALTER TABLE turn_actions
  ADD CONSTRAINT ck_turn_action_context_hint CHECK (
    context_hint IS NULL OR (
      jsonb_typeof(context_hint) = 'object'
      AND (context_hint - 'resource_ids' - 'region' - 'zone') = '{}'::jsonb
      AND (
        NOT (context_hint ? 'resource_ids')
        OR (
          jsonb_typeof(context_hint -> 'resource_ids') = 'array'
          AND jsonb_array_length(context_hint -> 'resource_ids') <= 16
          AND NOT jsonb_path_exists(
            context_hint,
            '$.resource_ids[*] ? (@.type() != "string")'
          )
        )
      )
      AND (
        NOT (context_hint ? 'region')
        OR jsonb_typeof(context_hint -> 'region') = 'string'
      )
      AND (
        NOT (context_hint ? 'zone')
        OR jsonb_typeof(context_hint -> 'zone') = 'string'
      )
    )
  );
