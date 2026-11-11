-- name: CreateSession :exec
INSERT INTO sessions (token, user_id, email, created_at, expires_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT * FROM sessions WHERE token = ? AND datetime(expires_at) > datetime('now');

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE datetime(expires_at) <= datetime('now');
