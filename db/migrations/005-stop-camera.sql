-- Per-stop camera view settings

ALTER TABLE stops ADD COLUMN cam_lng REAL;
ALTER TABLE stops ADD COLUMN cam_lat REAL;
ALTER TABLE stops ADD COLUMN cam_height REAL;
ALTER TABLE stops ADD COLUMN cam_heading REAL;
ALTER TABLE stops ADD COLUMN cam_pitch REAL;

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (005, '005-stop-camera');
