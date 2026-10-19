-- name: CreatePlugin :exec
INSERT INTO plugins (name, version, title, description, source, enabled, installed_at, updated_at)
VALUES (@name, @version, @title, @description, @source, @enabled, @installed_at, @updated_at);

-- name: GetPlugin :one
SELECT * FROM plugins WHERE name = @name;

-- name: ListPlugins :many
SELECT * FROM plugins ORDER BY name;

-- name: UpdatePlugin :execrows
UPDATE plugins
SET version = @version, title = @title, description = @description, source = @source, enabled = @enabled, updated_at = @updated_at
WHERE name = @name;

-- name: DeletePlugin :execrows
DELETE FROM plugins WHERE name = @name;
