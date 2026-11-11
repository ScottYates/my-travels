-- name: UpsertUser :exec
INSERT INTO users (user_id, email, first_seen, last_login)
VALUES (?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE
SET email = excluded.email,
    last_login = excluded.last_login;
