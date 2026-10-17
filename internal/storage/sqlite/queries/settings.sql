-- name: GetGlobalSettings :one
SELECT data FROM settings_global WHERE id = 1;

-- name: SaveGlobalSettings :exec
INSERT INTO settings_global (id, data, updated_at)
VALUES (1, @data, @updated_at)
ON CONFLICT (id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at;
