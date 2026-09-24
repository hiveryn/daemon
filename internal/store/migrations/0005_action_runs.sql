-- Durable history of Action executions, addressed by the stable execution id.
-- Not a foreign key to sessions: the record outlives its agent session, and it
-- is never pruned. The partial unique index is the global single-run rule: at
-- most one running execution per action.
CREATE TABLE action_runs (
    id TEXT PRIMARY KEY,
    action TEXT NOT NULL,
    trigger TEXT NOT NULL,
    status TEXT NOT NULL,
    prompt TEXT NOT NULL,
    profile_name TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    output_dir TEXT NOT NULL,
    session_id TEXT,
    summary TEXT,
    error TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    ended_at TEXT
);

CREATE UNIQUE INDEX idx_action_runs_one_running ON action_runs(action) WHERE status = 'running';
CREATE INDEX idx_action_runs_action_created ON action_runs(action, created_at DESC);
