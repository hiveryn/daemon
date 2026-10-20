-- No historical backfill: only future running -> terminal transitions owe notices.
ALTER TABLE action_runs ADD COLUMN notification_pending INTEGER NOT NULL DEFAULT 0;
