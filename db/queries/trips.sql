-- name: CreateTrip :exec
INSERT INTO trips (id, share_id, title, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetTrip :one
SELECT * FROM trips WHERE id = ?;

-- name: GetTripByShareID :one
SELECT * FROM trips WHERE share_id = ?;

-- name: ListTrips :many
SELECT * FROM trips ORDER BY updated_at DESC;

-- name: UpdateTrip :exec
UPDATE trips SET title = ?, description = ?, cover_photo_id = ?, updated_at = ? WHERE id = ?;

-- name: DeleteTrip :exec
DELETE FROM trips WHERE id = ?;

-- name: CreateStop :exec
INSERT INTO stops (id, trip_id, title, description, lat, lng, elevation, stop_order, arrived_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListStops :many
SELECT * FROM stops WHERE trip_id = ? ORDER BY stop_order ASC;

-- name: GetStop :one
SELECT * FROM stops WHERE id = ?;

-- name: UpdateStop :exec
UPDATE stops SET title = ?, description = ?, lat = ?, lng = ?, elevation = ?, stop_order = ?, arrived_at = ? WHERE id = ?;

-- name: DeleteStop :exec
DELETE FROM stops WHERE id = ?;

-- name: CreatePhoto :exec
INSERT INTO photos (id, trip_id, stop_id, filename, original_name, caption, lat, lng, taken_at, width, height, size_bytes, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListPhotos :many
SELECT * FROM photos WHERE trip_id = ? ORDER BY created_at ASC;

-- name: ListPhotosByStop :many
SELECT * FROM photos WHERE stop_id = ? ORDER BY created_at ASC;

-- name: GetPhoto :one
SELECT * FROM photos WHERE id = ?;

-- name: UpdatePhoto :exec
UPDATE photos SET stop_id = ?, caption = ?, lat = ?, lng = ? WHERE id = ?;

-- name: DeletePhoto :exec
DELETE FROM photos WHERE id = ?;

-- name: CreateRoute :exec
INSERT INTO routes (id, trip_id, name, geojson, color, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListRoutes :many
SELECT * FROM routes WHERE trip_id = ? ORDER BY created_at ASC;

-- name: DeleteRoute :exec
DELETE FROM routes WHERE id = ?;

-- name: CountStops :one
SELECT COUNT(*) FROM stops WHERE trip_id = ?;

-- name: CountPhotos :one
SELECT COUNT(*) FROM photos WHERE trip_id = ?;

-- name: MaxStopOrder :one
SELECT COALESCE(MAX(stop_order), -1) FROM stops WHERE trip_id = ?;
