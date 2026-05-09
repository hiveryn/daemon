CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    profile_name TEXT NOT NULL,
    architect_key TEXT NOT NULL,
    ticket_id TEXT,
    repo_key TEXT,
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
    status TEXT,
    tool TEXT,
    message TEXT,
    native_id TEXT,
    native_role TEXT,
    metadata TEXT,
    raw TEXT,
    at TEXT,
    UNIQUE(session_id, seq)
);
