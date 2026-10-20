ALTER TABLE action_runs ADD COLUMN artifact_mode TEXT NOT NULL DEFAULT 'none' CHECK (artifact_mode IN ('none', 'optional', 'required'));
UPDATE action_runs SET artifact_mode = 'required' WHERE artifact_contract <> '';
