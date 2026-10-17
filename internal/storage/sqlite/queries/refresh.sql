-- name: InsertRefreshAttempt :exec
INSERT INTO refresh_history (id, task_id, strategy, cause, outcome, error, old_url, new_url, downloaded, deadline, created_at)
VALUES (@id, @task_id, @strategy, @cause, @outcome, @error, @old_url, @new_url, @downloaded, @deadline, @created_at);

-- name: ListRefreshAttempts :many
SELECT * FROM refresh_history WHERE task_id = @task_id ORDER BY created_at, id;

-- name: LatestRefreshAttempt :one
SELECT * FROM refresh_history WHERE task_id = @task_id ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: CountRefreshAttemptsAt :one
SELECT count(*) FROM refresh_history
WHERE task_id = @task_id AND downloaded = @downloaded AND outcome IN ('success', 'failed', 'mismatch', 'restarted');

-- name: ListPendingAwaits :many
SELECT r.* FROM refresh_history r
JOIN tasks t ON t.id = r.task_id
WHERE t.status = 'awaiting_link'
  AND r.outcome = 'waiting'
  AND r.deadline IS NOT NULL
  AND r.id = (SELECT h.id FROM refresh_history h WHERE h.task_id = t.id ORDER BY h.created_at DESC, h.id DESC LIMIT 1)
ORDER BY r.deadline, r.id;
