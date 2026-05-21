CREATE TABLE session_intents (
    id TEXT PRIMARY KEY,
    architect_key TEXT NOT NULL,
    session_type TEXT NOT NULL,
    context_id TEXT NOT NULL,
    prompt TEXT NOT NULL,
    workdir TEXT NOT NULL,
    instructions TEXT,
    created_by TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE session_runs (
    id TEXT PRIMARY KEY,
    session_intent_id TEXT NOT NULL REFERENCES session_intents(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    profile_name TEXT NOT NULL,
    profile_snapshot TEXT,
    workdir TEXT NOT NULL,
    native_id TEXT,
    failure_reason TEXT,
    started_at TEXT,
    ended_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE session_events (
    id TEXT PRIMARY KEY,
    session_intent_id TEXT NOT NULL REFERENCES session_intents(id) ON DELETE CASCADE,
    run_id TEXT,
    seq INTEGER NOT NULL,
    type TEXT NOT NULL,
    status TEXT,
    tool TEXT,
    message TEXT,
    native_id TEXT,
    primary_native_id TEXT,
    native_session_role TEXT,
    metadata TEXT,
    raw TEXT,
    at TEXT NOT NULL
);

CREATE INDEX idx_session_intents_architect_type ON session_intents(architect_key, session_type);
CREATE INDEX idx_session_intents_context_type ON session_intents(context_id, session_type);
CREATE UNIQUE INDEX idx_session_runs_one_running_per_intent ON session_runs(session_intent_id) WHERE status = 'running';
CREATE INDEX idx_session_runs_intent_created ON session_runs(session_intent_id, created_at DESC, id DESC);
CREATE INDEX idx_session_runs_status ON session_runs(status);
CREATE UNIQUE INDEX idx_session_events_intent_run_seq ON session_events(session_intent_id, ifnull(run_id, ''), seq);
CREATE INDEX idx_session_events_intent_seq ON session_events(session_intent_id, seq);
