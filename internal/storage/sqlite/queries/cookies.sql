-- name: UpsertCookie :exec
INSERT INTO cookies (
    id, domain, name, path, partition_key, encrypted_val,
    secure, http_only, same_site, host_only, expires_at, created_at, updated_at
) VALUES (
    @id, @domain, @name, @path, @partition_key, @encrypted_val,
    @secure, @http_only, @same_site, @host_only, @expires_at, @created_at, @updated_at
)
ON CONFLICT (domain, name, path, partition_key) DO UPDATE SET
    encrypted_val = excluded.encrypted_val,
    secure = excluded.secure,
    http_only = excluded.http_only,
    same_site = excluded.same_site,
    host_only = excluded.host_only,
    expires_at = excluded.expires_at,
    updated_at = excluded.updated_at;

-- name: GetCookie :one
SELECT * FROM cookies WHERE id = @id;

-- name: DeleteCookie :execrows
DELETE FROM cookies WHERE id = @id;

-- name: DeleteExpiredCookies :execrows
DELETE FROM cookies WHERE expires_at IS NOT NULL AND expires_at <= @now;

-- name: ListCookies :many
SELECT * FROM cookies
WHERE (CAST(@domain AS text) = '' OR domain = @domain COLLATE NOCASE)
  AND (CAST(@name AS text) = '' OR name = @name)
  AND (CAST(@partition_key AS text) = '' OR partition_key = @partition_key)
ORDER BY domain, path, name;
