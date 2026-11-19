-- A task can be archived: kept, with its file and history, but out of every
-- listing and count except the archive's own. Archiving is not deleting, so
-- duplicate checks still see archived tasks.

-- +goose Up
ALTER TABLE tasks ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
CREATE INDEX tasks_archived ON tasks (archived);

-- +goose Down
DROP INDEX tasks_archived;
ALTER TABLE tasks DROP COLUMN archived;
