-- name: CreateTask :exec
INSERT INTO tasks (
    id, queue_id, status, priority, position, engine, name, dir, size, downloaded,
    source, resource, settings, labels, error, created_at, updated_at, started_at, completed_at
) VALUES (
    @id, @queue_id, @status, @priority, @position, @engine, @name, @dir, @size, @downloaded,
    @source, @resource, @settings, @labels, @error, @created_at, @updated_at, @started_at, @completed_at
);

-- name: GetTask :one
SELECT * FROM tasks WHERE id = @id;

-- name: UpdateTask :execrows
UPDATE tasks
SET queue_id = @queue_id,
    status = @status,
    priority = @priority,
    position = @position,
    engine = @engine,
    name = @name,
    dir = @dir,
    size = @size,
    downloaded = @downloaded,
    source = @source,
    resource = @resource,
    settings = @settings,
    labels = @labels,
    error = @error,
    updated_at = @updated_at,
    started_at = @started_at,
    completed_at = @completed_at
WHERE id = @id;

-- name: UpdateTaskProgress :execrows
UPDATE tasks
SET downloaded = @downloaded, size = @size, updated_at = @updated_at
WHERE id = @id;

-- name: DeleteTask :execrows
DELETE FROM tasks WHERE id = @id;

-- name: NextTaskPosition :one
SELECT CAST(COALESCE(MAX(position), -1) + 1 AS INTEGER) AS next
FROM tasks
WHERE queue_id = @queue_id;

-- name: CountTasksByStatus :many
SELECT status, COUNT(*) AS n FROM tasks GROUP BY status;

-- name: CountQueueTasksByStatus :many
SELECT status, COUNT(*) AS n FROM tasks WHERE queue_id = @queue_id GROUP BY status;

-- name: FindTaskBySourceURL :one
SELECT * FROM tasks
WHERE json_extract(source, '$.currentUrl') = @url OR json_extract(source, '$.originalUrl') = @url
ORDER BY created_at, id
LIMIT 1;

-- name: FindTaskByNameSize :one
SELECT * FROM tasks
WHERE name = @name AND size = @size AND status <> 'error'
ORDER BY created_at, id
LIMIT 1;

-- name: FindTaskByChecksum :one
SELECT * FROM tasks
WHERE json_extract(settings, '$.checksum.expected') = @expected
ORDER BY created_at, id
LIMIT 1;

-- name: StatsByEngine :many
SELECT engine, COUNT(*) AS n, CAST(COALESCE(SUM(downloaded), 0) AS INTEGER) AS bytes
FROM tasks GROUP BY engine ORDER BY engine;

-- name: StatsCompletedSince :one
SELECT COUNT(*) AS n, CAST(COALESCE(SUM(downloaded), 0) AS INTEGER) AS bytes
FROM tasks WHERE status = 'completed' AND completed_at >= @since;

-- name: StatsTotalDownloaded :one
SELECT CAST(COALESCE(SUM(downloaded), 0) AS INTEGER) AS bytes FROM tasks;
