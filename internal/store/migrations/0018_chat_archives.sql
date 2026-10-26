-- Retained independently of active ownership and provider launch secrets.
CREATE TABLE chat_archives (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL UNIQUE,
 architect_key TEXT NOT NULL,
 descriptor_json TEXT NOT NULL
);
CREATE INDEX chat_archive_project ON chat_archives(architect_key,seq);
