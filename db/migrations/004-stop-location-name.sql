-- Location name for stops (reverse-geocoded or user-edited)

ALTER TABLE stops ADD COLUMN location_name TEXT NOT NULL DEFAULT '';

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (004, '004-stop-location-name');
