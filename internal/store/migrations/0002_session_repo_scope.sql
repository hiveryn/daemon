ALTER TABLE sessions ADD COLUMN additional_repos TEXT NOT NULL DEFAULT '[]';
ALTER TABLE sessions ADD COLUMN additional_workdirs TEXT NOT NULL DEFAULT '[]';
ALTER TABLE session_runs ADD COLUMN additional_repos TEXT NOT NULL DEFAULT '[]';
ALTER TABLE session_runs ADD COLUMN additional_workdirs TEXT NOT NULL DEFAULT '[]';
