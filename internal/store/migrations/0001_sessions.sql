CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    profile_name TEXT NOT NULL,
    architect_key TEXT NOT NULL,
    prompt TEXT,
    instructions TEXT,
    status TEXT NOT NULL,
    native_id TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE session_events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
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
    at TEXT NOT NULL,
    UNIQUE(session_id, seq)
);

CREATE INDEX idx_sessions_architect_status ON sessions(architect_key, status);
CREATE UNIQUE INDEX idx_sessions_one_running_architect ON sessions(architect_key) WHERE status = 'running';
CREATE INDEX idx_session_events_session_seq ON session_events(session_id, seq);
