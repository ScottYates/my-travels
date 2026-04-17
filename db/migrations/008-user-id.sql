-- Add user_id to trips for per-user ownership
ALTER TABLE trips ADD COLUMN user_id TEXT;

CREATE INDEX IF NOT EXISTS idx_trips_user ON trips(user_id);

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (008, '008-user-id');
