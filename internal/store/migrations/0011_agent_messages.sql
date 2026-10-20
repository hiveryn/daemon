-- Durable outbox of messages delivered into agent sessions through their
-- provider's native route. Not a foreign key to sessions: a message addressed
-- to an architect outlives every session and waits for the next one. The
-- UNIQUE dedupe_key is the producer's identity of the announcement; retries,
-- replays and crash recovery converge on one row.
CREATE TABLE agent_messages (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    recipient TEXT NOT NULL,
    architect_key TEXT NOT NULL DEFAULT '',
    dedupe_key TEXT NOT NULL UNIQUE,
    body TEXT NOT NULL,
    payload TEXT,
    state TEXT NOT NULL,
    condition TEXT NOT NULL DEFAULT '',
    target_session_id TEXT NOT NULL DEFAULT '',
    target_agent TEXT NOT NULL DEFAULT '',
    target_native_id TEXT NOT NULL DEFAULT '',
    target_config_root TEXT NOT NULL DEFAULT '',
    target_workdir TEXT NOT NULL DEFAULT '',
    mechanism TEXT NOT NULL DEFAULT '',
    provider_ref TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    next_attempt_at TEXT,
    created_at TEXT NOT NULL,
    committed_at TEXT,
    handed_at TEXT,
    appeared_at TEXT,
    acknowledged_at TEXT,
    ended_at TEXT,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_agent_messages_state_created ON agent_messages(state, created_at);
CREATE INDEX idx_agent_messages_architect_created ON agent_messages(architect_key, created_at DESC);
