-- Migration for cookies table. Timestamps are INTEGER Unix milliseconds (UTC).

-- +goose Up
CREATE TABLE cookies (
    id            TEXT    PRIMARY KEY,
    domain        TEXT    NOT NULL COLLATE NOCASE,
    name          TEXT    NOT NULL,
    path          TEXT    NOT NULL,
    partition_key TEXT    NOT NULL DEFAULT '',
    encrypted_val BLOB    NOT NULL,
    secure        INTEGER NOT NULL DEFAULT 0,
    http_only     INTEGER NOT NULL DEFAULT 0,
    same_site     TEXT    NOT NULL DEFAULT '',
    host_only     INTEGER NOT NULL DEFAULT 0,
    expires_at    INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (domain, name, path, partition_key)
) STRICT;

CREATE INDEX cookies_domain_part ON cookies (domain, partition_key);
CREATE INDEX cookies_expires ON cookies (expires_at);

-- +goose Down
DROP TABLE cookies;
