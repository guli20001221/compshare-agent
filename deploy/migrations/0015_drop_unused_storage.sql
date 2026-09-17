-- Drops storage nothing writes or reads:
--   * the durable-turn tables of a retired executor (chat_turns, chat_turn_events,
--     conversation_leases, turn_actions, turn_interactions), which the migrations
--     that created them no longer ship;
--   * four agent_traces columns the writer stopped naming (intent, route_status,
--     refusal_type, resolution) and the two indexes over them.
--
-- A binary that still names those columns fails every trace INSERT once they
-- are gone, so apply this after deploying the binary that no longer does. All
-- statements are idempotent.

DROP TABLE IF EXISTS chat_turn_events;
DROP TABLE IF EXISTS turn_interactions;
DROP TABLE IF EXISTS turn_actions;
DROP TABLE IF EXISTS conversation_leases;
DROP TABLE IF EXISTS chat_turns;

DROP INDEX IF EXISTS idx_refusal_time;
DROP INDEX IF EXISTS idx_route_status_time;

ALTER TABLE agent_traces
  DROP COLUMN IF EXISTS intent,
  DROP COLUMN IF EXISTS route_status,
  DROP COLUMN IF EXISTS refusal_type,
  DROP COLUMN IF EXISTS resolution;
