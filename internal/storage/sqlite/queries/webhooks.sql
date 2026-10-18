-- name: CreateWebhook :exec
INSERT INTO webhooks (id, url, events, enabled, secret, created_at, updated_at)
VALUES (@id, @url, @events, @enabled, @secret, @created_at, @updated_at);

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE id = @id;

-- name: ListWebhooks :many
SELECT * FROM webhooks ORDER BY created_at, id;

-- name: UpdateWebhook :execrows
UPDATE webhooks
SET url = @url, events = @events, enabled = @enabled, secret = @secret, updated_at = @updated_at
WHERE id = @id;

-- name: DeleteWebhook :execrows
DELETE FROM webhooks WHERE id = @id;
