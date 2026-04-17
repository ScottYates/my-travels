-- Comments on photos

CREATE TABLE IF NOT EXISTS comments (
    id TEXT PRIMARY KEY,
    photo_id TEXT NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
    trip_id TEXT NOT NULL REFERENCES trips(id) ON DELETE CASCADE,
    author TEXT NOT NULL DEFAULT 'Anonymous',
    body TEXT NOT NULL,
    created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_comments_photo ON comments(photo_id);
CREATE INDEX IF NOT EXISTS idx_comments_trip ON comments(trip_id);

INSERT OR IGNORE INTO migrations (migration_number, migration_name)
VALUES (006, '006-comments');
