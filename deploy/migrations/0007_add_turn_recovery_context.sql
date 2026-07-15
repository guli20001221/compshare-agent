-- 0007_add_turn_recovery_context.sql (PostgreSQL)
--
-- A frozen, secret-free execution envelope lets another replica resume a turn
-- without asking the client to resend request-local state. Action context hints
-- are deliberately advisory and structurally incapable of carrying authority.

ALTER TABLE chat_turns
  ADD COLUMN execution_envelope JSONB;

ALTER TABLE chat_turns
  ADD CONSTRAINT ck_chat_turn_execution_envelope CHECK (
    execution_envelope IS NULL OR jsonb_typeof(execution_envelope) = 'object'
  );

ALTER TABLE turn_actions
  ADD COLUMN context_hint JSONB;

ALTER TABLE turn_actions
  ADD CONSTRAINT ck_turn_action_context_hint CHECK (
    context_hint IS NULL OR (
      jsonb_typeof(context_hint) = 'object'
      AND (context_hint - 'resource_ids' - 'region' - 'zone') = '{}'::jsonb
      AND (
        NOT (context_hint ? 'resource_ids')
        OR jsonb_typeof(context_hint -> 'resource_ids') = 'array'
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

CREATE INDEX idx_chat_turns_recovery
  ON chat_turns (status, updated_at)
  WHERE execution_envelope IS NOT NULL
    AND status IN (
      'accepted', 'running', 'awaiting_confirmation', 'committing',
      'failed_retryable'
    );
