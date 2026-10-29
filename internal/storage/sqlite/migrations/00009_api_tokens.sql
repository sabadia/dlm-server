-- Named API tokens besides the primary one in the token file. Only a SHA-256
-- hash of the token is stored: the token itself is shown once, when it is
-- created. Timestamps are INTEGER Unix milliseconds (UTC).

-- +goose Up
CREATE TABLE api_tokens (
    id           TEXT    PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    scope        TEXT    NOT NULL,
    hash         TEXT    NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
) STRICT;

-- +goose Down
DROP TABLE api_tokens;
