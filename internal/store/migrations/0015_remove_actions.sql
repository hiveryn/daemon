-- The Actions feature was removed. Drop its execution history and the
-- ownership records of its managed artifact directories, and retire the
-- runtime rows that existed only for it: action sessions (with their runs,
-- events and remote resources), action_ended outbox messages, executeAction
-- deferred-intent records and executeAction approval events in other sessions'
-- logs (intent events are exempt from the event ring, so they would otherwise
-- stay forever). Ticket, architect, question and other message data is kept.
-- Files on disk — HIVERYN_HOME/action-runs, HIVERYN_HOME/action-artifacts and
-- remote ~/.local/state/hiveryn-actions — are private user data and are not
-- touched here.
DELETE FROM remote_resources WHERE session_id IN (SELECT id FROM sessions WHERE session_type = 'action');
DELETE FROM session_events WHERE session_id IN (SELECT id FROM sessions WHERE session_type = 'action');
DELETE FROM session_runs WHERE session_id IN (SELECT id FROM sessions WHERE session_type = 'action');
DELETE FROM sessions WHERE session_type = 'action';
DELETE FROM session_events WHERE type = 'intent' AND json_valid(raw) AND json_extract(raw, '$.intent_type') = 'executeAction';
DELETE FROM agent_messages WHERE kind = 'action_ended';
DELETE FROM deferred_intents WHERE intent_type = 'executeAction';
DROP TABLE action_artifacts;
DROP TABLE action_runs;
