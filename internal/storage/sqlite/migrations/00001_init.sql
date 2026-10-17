-- Initial schema. Timestamps are INTEGER Unix milliseconds (UTC); JSON
-- documents are stored as TEXT.

-- +goose Up
CREATE TABLE settings_global (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    data       TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE queues (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    is_default INTEGER NOT NULL,
    position   INTEGER NOT NULL,
    state      TEXT    NOT NULL,
    settings   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE tasks (
    id           TEXT    PRIMARY KEY,
    queue_id     TEXT    NOT NULL REFERENCES queues (id) ON DELETE RESTRICT,
    status       TEXT    NOT NULL,
    priority     INTEGER NOT NULL,
    position     INTEGER NOT NULL,
    engine       TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    dir          TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    downloaded   INTEGER NOT NULL,
    source       TEXT    NOT NULL,
    resource     TEXT,
    settings     TEXT    NOT NULL,
    labels       TEXT    NOT NULL,
    error        TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    started_at   INTEGER,
    completed_at INTEGER
) STRICT;

CREATE INDEX tasks_queue_status ON tasks (queue_id, status);
CREATE INDEX tasks_queue_order ON tasks (queue_id, priority DESC, position, created_at);
CREATE INDEX tasks_created ON tasks (created_at DESC, id);

CREATE TABLE task_segments (
    task_id TEXT    NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    idx     INTEGER NOT NULL,
    start   INTEGER NOT NULL,
    "end"   INTEGER NOT NULL,
    done    INTEGER NOT NULL,
    PRIMARY KEY (task_id, idx)
) STRICT;

CREATE TABLE engine_state (
    task_id    TEXT    PRIMARY KEY REFERENCES tasks (id) ON DELETE CASCADE,
    state      BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE engine_state;
DROP TABLE task_segments;
DROP TABLE tasks;
DROP TABLE queues;
DROP TABLE settings_global;
