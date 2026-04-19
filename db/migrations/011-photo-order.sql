-- Add photo_order to photos table for custom ordering within stops
ALTER TABLE photos ADD COLUMN photo_order INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (011, '011-photo-order');
