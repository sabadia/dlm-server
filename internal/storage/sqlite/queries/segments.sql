-- name: DeleteSegments :exec
DELETE FROM task_segments WHERE task_id = @task_id;

-- name: InsertSegment :exec
INSERT INTO task_segments (task_id, idx, start, "end", done)
VALUES (@task_id, @idx, @start, @end_offset, @done);

-- name: ListSegments :many
SELECT idx, start, "end", done FROM task_segments WHERE task_id = @task_id ORDER BY idx;

-- name: GetEngineState :one
SELECT state FROM engine_state WHERE task_id = @task_id;

-- name: PutEngineState :exec
INSERT INTO engine_state (task_id, state, updated_at)
VALUES (@task_id, @state, @updated_at)
ON CONFLICT (task_id) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at;

-- name: DeleteEngineState :exec
DELETE FROM engine_state WHERE task_id = @task_id;
