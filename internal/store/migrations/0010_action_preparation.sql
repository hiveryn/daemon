-- Freeze the launch contract; older runs all promised artifacts.
ALTER TABLE action_runs ADD COLUMN artifact_contract TEXT NOT NULL DEFAULT 'Legacy artifact package';
