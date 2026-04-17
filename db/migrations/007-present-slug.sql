-- Add a user-chosen slug for presentation share links
ALTER TABLE trips ADD COLUMN present_slug TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_trips_present_slug ON trips(present_slug) WHERE present_slug IS NOT NULL;

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (007, '007-present-slug');
