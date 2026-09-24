-- Architect-requested Action executions. A request is recorded pending_approval
-- under the execution id its deferred approval returned, and keeps that id when
-- it starts. architect_key scopes who may read the result (empty for manual
-- launches); requester_session_id names the architect session that asked;
-- reason keeps the user's denial reason.
ALTER TABLE action_runs ADD COLUMN architect_key TEXT;
ALTER TABLE action_runs ADD COLUMN requester_session_id TEXT;
ALTER TABLE action_runs ADD COLUMN reason TEXT;

CREATE INDEX idx_action_runs_status ON action_runs(status);
