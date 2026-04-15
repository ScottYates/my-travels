-- Trips and related tables

CREATE TABLE IF NOT EXISTS trips (
    id TEXT PRIMARY KEY,
    share_id TEXT UNIQUE NOT NULL,
    title TEXT NOT NULL DEFAULT 'Untitled Trip',
    description TEXT NOT NULL DEFAULT '',
    cover_photo_id TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS stops (
    id TEXT PRIMARY KEY,
    trip_id TEXT NOT NULL REFERENCES trips(id) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    lat REAL NOT NULL,
    lng REAL NOT NULL,
    elevation REAL NOT NULL DEFAULT 0,
    stop_order INTEGER NOT NULL DEFAULT 0,
    arrived_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS photos (
    id TEXT PRIMARY KEY,
    trip_id TEXT NOT NULL REFERENCES trips(id) ON DELETE CASCADE,
    stop_id TEXT REFERENCES stops(id) ON DELETE SET NULL,
    filename TEXT NOT NULL,
    original_name TEXT NOT NULL DEFAULT '',
    caption TEXT NOT NULL DEFAULT '',
    lat REAL,
    lng REAL,
    taken_at TIMESTAMP,
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS routes (
    id TEXT PRIMARY KEY,
    trip_id TEXT NOT NULL REFERENCES trips(id) ON DELETE CASCADE,
    name TEXT NOT NULL DEFAULT '',
    geojson TEXT NOT NULL DEFAULT '{}',
    color TEXT NOT NULL DEFAULT '#3b82f6',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_stops_trip ON stops(trip_id);
CREATE INDEX IF NOT EXISTS idx_photos_trip ON photos(trip_id);
CREATE INDEX IF NOT EXISTS idx_photos_stop ON photos(stop_id);
CREATE INDEX IF NOT EXISTS idx_routes_trip ON routes(trip_id);
CREATE INDEX IF NOT EXISTS idx_trips_share ON trips(share_id);

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (002, '002-trips');
