-- Per-attempt record of link refreshes. Timestamps are INTEGER Unix milliseconds (UTC).

-- +goose Up
CREATE TABLE refresh_history (
    id         TEXT    PRIMARY KEY,
    task_id    TEXT    NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    strategy   TEXT    NOT NULL,
    cause      TEXT    NOT NULL,
    outcome    TEXT    NOT NULL,
    error      TEXT    NOT NULL DEFAULT '',
    old_url    TEXT    NOT NULL DEFAULT '',
    new_url    TEXT    NOT NULL DEFAULT '',
    downloaded INTEGER NOT NULL DEFAULT 0,
    deadline   INTEGER,
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX refresh_history_task ON refresh_history (task_id, created_at, id);

-- +goose Down
DROP TABLE refresh_history;
