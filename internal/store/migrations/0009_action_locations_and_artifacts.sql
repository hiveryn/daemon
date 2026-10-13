-- Actions run where actions.yaml registers them: machine is the machines.yaml
-- key ('' = local). delivery_dir is the folder a completed execution's agent
-- concluded with.
ALTER TABLE action_runs ADD COLUMN machine TEXT NOT NULL DEFAULT '';
ALTER TABLE action_runs ADD COLUMN delivery_dir TEXT;

-- Ownership records of Hiveryn-managed artifact directories: the frozen
-- package of an execution on its machine, and verified copies for callers on
-- other machines. A record is written before its directory is created, so an
-- interrupted copy or an expired directory can always be found and removed.
-- Not a foreign key: like action_runs, the records outlive sessions, and an
-- expired one is kept (state expired) with the execution history.
CREATE TABLE action_artifacts (
    execution_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    machine TEXT NOT NULL,
    ssh TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    managed_root TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    error TEXT,
    cleanup_error TEXT,
    files INTEGER NOT NULL DEFAULT 0,
    bytes INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    ready_at TEXT,
    expires_at TEXT,
    PRIMARY KEY (execution_id, kind, machine)
);
CREATE INDEX idx_action_artifacts_state ON action_artifacts(state);

-- Executions completed before frozen packages delivered into their output
-- directory. That folder stays their package, in place: it is not a managed
-- copy (managed_root ''), it never expires and nothing removes it.
INSERT INTO action_artifacts (execution_id, kind, machine, ssh, path, managed_root, state, created_at, ready_at)
SELECT id, 'package', '', '', output_dir, '', 'ready', COALESCE(ended_at, created_at), ended_at
FROM action_runs
WHERE status = 'completed' AND output_dir != '';
