-- name: CreateQueue :exec
INSERT INTO queues (id, name, is_default, position, state, settings, created_at, updated_at)
VALUES (@id, @name, @is_default, @position, @state, @settings, @created_at, @updated_at);

-- name: GetQueue :one
SELECT * FROM queues WHERE id = @id;

-- name: ListQueues :many
SELECT * FROM queues ORDER BY position, created_at, id;

-- name: UpdateQueue :execrows
UPDATE queues
SET name = @name,
    is_default = @is_default,
    position = @position,
    state = @state,
    settings = @settings,
    updated_at = @updated_at
WHERE id = @id;

-- name: DeleteQueue :execrows
DELETE FROM queues WHERE id = @id;

-- name: NextQueuePosition :one
SELECT CAST(COALESCE(MAX(position), -1) + 1 AS INTEGER) AS next FROM queues;

