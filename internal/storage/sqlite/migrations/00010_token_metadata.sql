-- What is known about the client a named token was issued to. Only tokens
-- created by pairing a browser extension carry it; the others leave the columns
-- empty.

-- +goose Up
ALTER TABLE api_tokens ADD COLUMN kind          TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN browser       TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN client_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN client_version TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE api_tokens DROP COLUMN client_version;
ALTER TABLE api_tokens DROP COLUMN client_id;
ALTER TABLE api_tokens DROP COLUMN browser;
ALTER TABLE api_tokens DROP COLUMN kind;
