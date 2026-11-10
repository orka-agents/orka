-- SQLite transition: 173-to-174 (schema version 6 to 7).
-- Adds the Agent promotion log (orka.agent.promotion.v1): one record per
-- promote or rollback of a stable Agent, at most one open (pending or
-- applying) per Agent. Records are kept after the Agent is deleted. It adds
-- no rows. Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS agent_promotions (
  seq         INTEGER PRIMARY KEY,
  namespace   TEXT NOT NULL,
  agent_uid   TEXT NOT NULL,
  id          TEXT NOT NULL,
  state       TEXT NOT NULL,
  record_json TEXT NOT NULL,
  created_at  TIMESTAMP NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_promotions_id ON agent_promotions(namespace, id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_promotions_open ON agent_promotions(namespace, agent_uid) WHERE state IN ('pending', 'applying');
