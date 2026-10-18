-- name: CreateCategory :exec
INSERT INTO categories (id, name, position, data, created_at, updated_at)
VALUES (@id, @name, @position, @data, @created_at, @updated_at);

-- name: GetCategory :one
SELECT * FROM categories WHERE id = @id;

-- name: ListCategories :many
SELECT * FROM categories ORDER BY position, id;

-- name: UpdateCategory :execrows
UPDATE categories
SET name = @name, position = @position, data = @data, updated_at = @updated_at
WHERE id = @id;

-- name: DeleteCategory :execrows
DELETE FROM categories WHERE id = @id;

-- name: NextCategoryPosition :one
SELECT CAST(COALESCE(MAX(position), -1) + 1 AS INTEGER) AS next FROM categories;
