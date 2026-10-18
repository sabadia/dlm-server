-- Categories sort downloads into directories (the rules and the directory are
-- one JSON document), and indexes for duplicate detection. Timestamps are
-- INTEGER Unix milliseconds (UTC).

-- +goose Up
CREATE TABLE categories (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    position   INTEGER NOT NULL,
    data       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE INDEX categories_position ON categories (position, id);

-- Duplicate detection looks tasks up by link, by name and size, and by the
-- checksum they expect.
CREATE INDEX tasks_source_url ON tasks (json_extract(source, '$.currentUrl'));
CREATE INDEX tasks_original_url ON tasks (json_extract(source, '$.originalUrl'));
CREATE INDEX tasks_name_size ON tasks (name, size);
CREATE INDEX tasks_checksum ON tasks (json_extract(settings, '$.checksum.expected'));

-- +goose Down
DROP INDEX tasks_checksum;
DROP INDEX tasks_name_size;
DROP INDEX tasks_original_url;
DROP INDEX tasks_source_url;
DROP TABLE categories;
