-- name: CreateTrip :exec
INSERT INTO trips (id, share_id, title, description, created_at, updated_at, user_id)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetTrip :one
SELECT * FROM trips WHERE id = ?;

-- name: GetTripByShareID :one
SELECT * FROM trips WHERE share_id = ?;

-- name: ListTrips :many
SELECT * FROM trips ORDER BY updated_at DESC;

-- name: ListTripsByUser :many
SELECT * FROM trips WHERE user_id = ? ORDER BY updated_at DESC;

-- name: UpdateTrip :exec
UPDATE trips SET title = ?, description = ?, cover_photo_id = ?, default_cam_heading = ?, default_cam_pitch = ?, default_cam_range = ?, updated_at = ? WHERE id = ?;

-- name: DeleteTrip :exec
DELETE FROM trips WHERE id = ?;

-- name: CreateStop :exec
INSERT INTO stops (id, trip_id, title, description, lat, lng, elevation, stop_order, arrived_at, created_at, location_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListStops :many
SELECT * FROM stops WHERE trip_id = ? ORDER BY stop_order ASC;

-- name: GetStop :one
SELECT * FROM stops WHERE id = ?;

-- name: UpdateStop :exec
UPDATE stops SET title = ?, description = ?, lat = ?, lng = ?, elevation = ?, stop_order = ?, arrived_at = ?, location_name = ?, cam_lng = ?, cam_lat = ?, cam_height = ?, cam_heading = ?, cam_pitch = ? WHERE id = ?;

-- name: DeleteStop :exec
DELETE FROM stops WHERE id = ?;

-- name: CreatePhoto :exec
INSERT INTO photos (id, trip_id, stop_id, filename, original_name, caption, lat, lng, taken_at, width, height, size_bytes, created_at, is_video)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListPhotos :many
SELECT * FROM photos WHERE trip_id = ? ORDER BY photo_order ASC, created_at ASC;

-- name: ListPhotosByStop :many
SELECT * FROM photos WHERE stop_id = ? ORDER BY photo_order ASC, created_at ASC;

-- name: GetPhoto :one
SELECT * FROM photos WHERE id = ?;

-- name: UpdatePhoto :exec
UPDATE photos SET stop_id = ?, caption = ?, lat = ?, lng = ?, cam_heading = ?, cam_pitch = ?, cam_range = ? WHERE id = ?;

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

-- name: DeleteStopsByTrip :exec
DELETE FROM stops WHERE trip_id = ?;

-- name: DeletePhotosByTrip :exec
DELETE FROM photos WHERE trip_id = ?;

-- name: DeleteRoutesByTrip :exec
DELETE FROM routes WHERE trip_id = ?;

-- name: ResetTripDefaults :exec
UPDATE trips SET cover_photo_id = NULL, default_cam_heading = NULL, default_cam_pitch = NULL, default_cam_range = NULL, updated_at = ? WHERE id = ?;

-- name: SetPhotoStopID :exec
UPDATE photos SET stop_id = ? WHERE id = ?;

-- name: UpdatePhotoOrder :exec
UPDATE photos SET photo_order = ? WHERE id = ?;

-- name: SetPhotoStopAndOrder :exec
UPDATE photos SET stop_id = ?, photo_order = ? WHERE id = ?;

-- name: ClearPhotoStopIDs :exec
UPDATE photos SET stop_id = NULL WHERE trip_id = ?;

-- name: ListPhotosWithLocation :many
SELECT * FROM photos WHERE trip_id = ? AND lat IS NOT NULL AND lng IS NOT NULL ORDER BY taken_at ASC, created_at ASC;

-- name: CreateComment :exec
INSERT INTO comments (id, photo_id, trip_id, author, body, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListCommentsByPhoto :many
SELECT * FROM comments WHERE photo_id = ? ORDER BY created_at ASC;

-- name: ListCommentsByTrip :many
SELECT * FROM comments WHERE trip_id = ? ORDER BY created_at ASC;

-- name: DeleteComment :exec
DELETE FROM comments WHERE id = ?;

-- name: UpdateComment :exec
UPDATE comments SET author = ?, body = ? WHERE id = ?;

-- name: GetComment :one
SELECT * FROM comments WHERE id = ?;

-- name: GetTripByPresentSlug :one
SELECT * FROM trips WHERE present_slug = ?;

-- name: UpdatePresentSlug :exec
UPDATE trips SET present_slug = ?, updated_at = ? WHERE id = ?;

-- name: ClaimOrphanedTrips :exec
UPDATE trips SET user_id = ? WHERE user_id IS NULL;

-- name: ShiftStopOrders :exec
UPDATE stops SET stop_order = stop_order + 1 WHERE trip_id = ? AND stop_order >= ?;
