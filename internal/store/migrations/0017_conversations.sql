ALTER TABLE sessions ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'terminal' CHECK(interaction_mode IN ('terminal','chat'));
-- Conversation receipts intentionally outlive session deletion, so a client can
-- poll a conclusion/discard disposition. They contain synthetic/user text only.
CREATE TABLE conversation_submissions (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 session_id TEXT NOT NULL,
 request_id TEXT NOT NULL,
 origin TEXT NOT NULL,
 text TEXT NOT NULL,
 status TEXT NOT NULL,
 native_turn_id TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(session_id,request_id)
);
CREATE INDEX conversation_queue ON conversation_submissions(session_id,seq);
-- Append-only versions make revision-bounded pagination a real snapshot even
-- when an old message changes between page requests.
CREATE TABLE conversation_items (
 revision INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL,
 item_id TEXT NOT NULL,
 item_json TEXT NOT NULL
);
CREATE INDEX conversation_history ON conversation_items(session_id,revision);
