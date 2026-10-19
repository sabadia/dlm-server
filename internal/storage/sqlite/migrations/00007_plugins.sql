-- Installed plugins. The plugin's files live in the data directory; this table
-- remembers what was installed, from where, and whether it is switched on.
-- Timestamps are INTEGER Unix milliseconds (UTC).

-- +goose Up
CREATE TABLE plugins (
    name         TEXT    PRIMARY KEY,
    version      TEXT    NOT NULL,
    title        TEXT    NOT NULL,
    description  TEXT    NOT NULL,
    source       TEXT    NOT NULL,
    enabled      INTEGER NOT NULL,
    installed_at INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE plugins;
