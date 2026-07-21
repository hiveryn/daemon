CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    architect_key TEXT NOT NULL,
    session_type TEXT NOT NULL,
    context_id TEXT NOT NULL,
    prompt TEXT NOT NULL,
    workdir TEXT NOT NULL,
    additional_repos TEXT NOT NULL DEFAULT '[]',
    additional_workdirs TEXT NOT NULL DEFAULT '[]',
    instructions TEXT,
    created_by TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE session_runs (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    profile_name TEXT NOT NULL,
    profile_snapshot TEXT,
    workdir TEXT NOT NULL,
    additional_repos TEXT NOT NULL DEFAULT '[]',
    additional_workdirs TEXT NOT NULL DEFAULT '[]',
    agent_status TEXT,
    native_id TEXT,
    failure_reason TEXT,
    started_at TEXT,
    ended_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE session_events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
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

CREATE INDEX idx_sessions_architect_type ON sessions(architect_key, session_type);
CREATE INDEX idx_sessions_context_type ON sessions(context_id, session_type);
CREATE UNIQUE INDEX idx_session_runs_one_running_per_session ON session_runs(session_id) WHERE status = 'running';
CREATE INDEX idx_session_runs_session_created ON session_runs(session_id, created_at DESC, id DESC);
CREATE INDEX idx_session_runs_status ON session_runs(status);
CREATE UNIQUE INDEX idx_session_events_session_run_seq ON session_events(session_id, ifnull(run_id, ''), seq);
CREATE INDEX idx_session_events_session_seq ON session_events(session_id, seq);
