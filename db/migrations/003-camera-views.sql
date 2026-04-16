-- Camera view settings for photo fly-to and trip defaults

-- Per-photo camera view (heading, pitch, range relative to photo position)
ALTER TABLE photos ADD COLUMN cam_heading REAL;
ALTER TABLE photos ADD COLUMN cam_pitch REAL;
ALTER TABLE photos ADD COLUMN cam_range REAL;

-- Trip-level default camera view for all photos
ALTER TABLE trips ADD COLUMN default_cam_heading REAL;
ALTER TABLE trips ADD COLUMN default_cam_pitch REAL;
ALTER TABLE trips ADD COLUMN default_cam_range REAL;

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (003, '003-camera-views');
