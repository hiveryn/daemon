-- Conversation-API input into existing terminal-owned sessions. The provider
-- reference (OpenCode message id; Claude/Codex use the submission id) and the
-- hand-off time are reserved durably BEFORE the native hand-off, so restart
-- reconciliation can look the input up instead of sending it again.
ALTER TABLE conversation_submissions ADD COLUMN provider_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_submissions ADD COLUMN handed_at TEXT NOT NULL DEFAULT '';
-- The provider process (main terminal) that received it: a replaced process
-- never completes a turn that was running when it died.
ALTER TABLE conversation_submissions ADD COLUMN handed_terminal TEXT NOT NULL DEFAULT '';
