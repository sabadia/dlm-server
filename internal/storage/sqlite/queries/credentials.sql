-- name: CreateCredential :exec
INSERT INTO credentials (id, host, scheme, username, password, created_at, updated_at)
VALUES (@id, @host, @scheme, @username, @password, @created_at, @updated_at);

-- name: GetCredential :one
SELECT * FROM credentials WHERE id = @id;

-- name: ListCredentials :many
SELECT * FROM credentials ORDER BY host, id;

-- name: UpdateCredential :execrows
UPDATE credentials
SET host = @host, scheme = @scheme, username = @username, password = @password, updated_at = @updated_at
WHERE id = @id;

-- name: DeleteCredential :execrows
DELETE FROM credentials WHERE id = @id;
