-- Users who have logged in.
--
-- Until now the only record of a user's email was their session row, and
-- expired sessions are deleted. The admin user list was built from
-- sessions, so a user (and their trips) dropped out of it once their
-- sessions had been purged. This table keeps one row per user for good.
CREATE TABLE IF NOT EXISTS users (
    user_id TEXT PRIMARY KEY,
    email TEXT NOT NULL,
    first_seen TEXT NOT NULL DEFAULT (datetime('now')),
    last_login TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Backfill from the sessions that still exist.
INSERT OR IGNORE INTO users (user_id, email, first_seen, last_login)
SELECT user_id, email, MIN(created_at), MAX(created_at)
FROM sessions
GROUP BY user_id;

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (012, '012-users');
