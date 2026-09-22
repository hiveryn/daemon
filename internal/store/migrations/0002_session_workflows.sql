-- Explicit workflow selection of a ticket session: a JSON array of canonical
-- absolute paths into the architect workspace's workflows/ directory. Empty is
-- a valid selection. The files are read live by the worker and never copied.
ALTER TABLE sessions ADD COLUMN workflows TEXT NOT NULL DEFAULT '[]';
