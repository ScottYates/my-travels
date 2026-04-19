-- Add is_video flag to photos table
ALTER TABLE photos ADD COLUMN is_video INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (010, '010-video');
