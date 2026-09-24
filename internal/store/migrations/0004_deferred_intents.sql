-- Durable outcomes of deferred (manual-approval) intents, addressable by the
-- intent id the request returned. Deliberately not a foreign key to sessions:
-- an outcome must stay retrievable after its session is gone. The in-memory
-- intent (and its captured operation) never survives a restart; startup fails
-- any record still open instead of leaving it falsely pending.
CREATE TABLE deferred_intents (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    intent_type TEXT NOT NULL,
    summary TEXT NOT NULL,
    payload TEXT,
    origin TEXT NOT NULL,
    status TEXT NOT NULL,
    inputs TEXT,
    result TEXT,
    reason TEXT,
    error TEXT,
    created_at TEXT NOT NULL,
    approved_at TEXT,
    ended_at TEXT
);

CREATE INDEX idx_deferred_intents_status ON deferred_intents(status);
CREATE INDEX idx_deferred_intents_ended ON deferred_intents(ended_at) WHERE ended_at IS NOT NULL;
