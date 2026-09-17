-- agent_traces.intent has a reader outside this repository: the console's
-- "AI 对话记录" page selects it for every session row and for its intent
-- filter, through this shared database. The trace writer never names the
-- column, so every row carries NULL, which that page handles the same way as
-- the empty string it used to receive.

ALTER TABLE agent_traces ADD COLUMN IF NOT EXISTS intent VARCHAR(32);
