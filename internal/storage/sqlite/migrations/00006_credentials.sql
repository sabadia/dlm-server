-- Stored site logins (HTTP Basic/Digest). The password is stored sealed
-- (AES-GCM, by the service). Timestamps are INTEGER Unix milliseconds (UTC).

-- +goose Up
CREATE TABLE credentials (
    id         TEXT    PRIMARY KEY,
    host       TEXT    NOT NULL,
    scheme     TEXT    NOT NULL,
    username   TEXT    NOT NULL,
    password   BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX credentials_host ON credentials (host);

-- +goose Down
DROP TABLE credentials;
