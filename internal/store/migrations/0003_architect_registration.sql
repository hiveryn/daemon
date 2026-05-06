CREATE TABLE architect_groups (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE architects (
    id             TEXT PRIMARY KEY,
    path           TEXT NOT NULL UNIQUE,
    title          TEXT,
    group_id       TEXT REFERENCES architect_groups(id) ON DELETE SET NULL,
    last_opened_at TEXT,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE repos (
    id           TEXT PRIMARY KEY,
    architect_id TEXT NOT NULL REFERENCES architects(id) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    path         TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(architect_id, key)
);
