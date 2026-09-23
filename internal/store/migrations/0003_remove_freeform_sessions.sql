-- Freeform sessions were removed; only architect and ticket sessions remain.
-- Drop any leftover freeform records so restore never meets a session type it
-- no longer understands. Their prompts and conclusions stay in the architect
-- workspace's freeform/ folder, which is user data and is not touched here.
DELETE FROM session_events WHERE session_id IN (SELECT id FROM sessions WHERE session_type = 'freeform');
DELETE FROM session_runs WHERE session_id IN (SELECT id FROM sessions WHERE session_type = 'freeform');
DELETE FROM sessions WHERE session_type = 'freeform';
