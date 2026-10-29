-- Bytes transferred per hour, queue and engine, for the statistics timeline.
-- bucket is the start of the hour as INTEGER Unix milliseconds (UTC); down is
-- what was downloaded, up what torrents uploaded.

-- +goose Up
CREATE TABLE transfer_buckets (
    bucket   INTEGER NOT NULL,
    queue_id TEXT    NOT NULL,
    engine   TEXT    NOT NULL,
    down     INTEGER NOT NULL,
    up       INTEGER NOT NULL,
    PRIMARY KEY (bucket, queue_id, engine)
) STRICT;

-- +goose Down
DROP TABLE transfer_buckets;
