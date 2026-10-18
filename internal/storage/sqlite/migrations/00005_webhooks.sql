-- Webhook endpoints that receive events. The secret that signs the deliveries
-- is stored sealed (AES-GCM, by the service). Timestamps are INTEGER Unix
-- milliseconds (UTC).

-- +goose Up
CREATE TABLE webhooks (
    id         TEXT    PRIMARY KEY,
    url        TEXT    NOT NULL,
    events     TEXT    NOT NULL,
    enabled    INTEGER NOT NULL,
    secret     BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE INDEX webhooks_created ON webhooks (created_at, id);

-- +goose Down
DROP TABLE webhooks;
