-- Companions: sessions started with an architect session and owned by it.
ALTER TABLE sessions ADD COLUMN owner_session_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_sessions_owner ON sessions(owner_session_id) WHERE owner_session_id <> '';

-- One row per configured companion of one architect session: the
-- configuration snapshot taken at that session's start and its lifecycle.
-- Removed with the architect session.
CREATE TABLE companions (
    architect_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    architect_key TEXT NOT NULL,
    repo TEXT NOT NULL,
    workdir TEXT NOT NULL,
    variant TEXT NOT NULL,
    agent TEXT NOT NULL DEFAULT '',
    machine TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (architect_session_id, name)
);
CREATE INDEX idx_companions_session ON companions(session_id) WHERE session_id <> '';

-- Architect-issued shutdown requests, bound to the exact architect and
-- companion sessions. At most one per companion session. Kept after both
-- sessions end so a reused request id stays recognizable.
CREATE TABLE companion_shutdowns (
    request_id TEXT PRIMARY KEY,
    architect_key TEXT NOT NULL,
    architect_session_id TEXT NOT NULL,
    companion_session_id TEXT NOT NULL UNIQUE,
    companion TEXT NOT NULL,
    status TEXT NOT NULL,
    handoff TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    requested_at TEXT NOT NULL,
    completed_at TEXT,
    stopped_at TEXT
);
