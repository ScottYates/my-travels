package srv

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"srv.exe.dev/db/dbgen"
)

// ---------------------------------------------------------------------------
// Resort Photos Into Stops
// ---------------------------------------------------------------------------
//
// Completely rebuilds stops from scratch by walking photos in chronological
// order and clustering them spatially.  A new cluster (== stop) starts
// whenever a photo is farther than `radius` from the current cluster's
// centroid.  This means revisiting the same area creates a NEW stop,
// preserving the traveller's actual itinerary.
//
// The radius (in km) is configurable via the request body; default 10 km.
//
// After building clusters the handler:
//   1. Deletes old stops and un-assigns all photos.
//   2. Creates new stops from the clusters.
//   3. Reverse-geocodes each stop at city level (zoom=5).
//   4. Assigns photos to their cluster stop, ordered by taken_at.
//   5. Unassigned photos (no GPS) get photo_order but no stop.
// ---------------------------------------------------------------------------

const defaultResortRadiusKm = 10.0

func (s *Server) handleResortPhotos(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	trip, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
		return
	}

	// Parse optional radius from body.
	var body struct {
		RadiusKm *float64 `json:"radius_km"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body) // ignore errors; defaults are fine
	}
	radiusKm := defaultResortRadiusKm
	if body.RadiusKm != nil && *body.RadiusKm > 0 {
		radiusKm = *body.RadiusKm
	}
	radiusMeters := radiusKm * 1000.0

	ctx := r.Context()
	q := dbgen.New(s.DB)

	// Fetch all photos, sorted by time then created_at.
	photos, err := q.ListPhotos(ctx, trip.ID)
	if err != nil {
		log.Printf("resort: list photos: %v", err)
		jsonError(w, "failed to list photos", http.StatusInternalServerError)
		return
	}

	log.Printf("resort: trip %s — %d photos, radius %.1f km", trip.ID, len(photos), radiusKm)

	// Sort ALL photos by taken_at (nulls last), then created_at.
	sortPhotosByTime(photos)

	// ----- Build clusters by walking in time order -----
	type cluster struct {
		photos []dbgen.Photo
		latSum float64
		lngSum float64
		nGPS   int
	}
	centroid := func(c *cluster) (float64, float64) {
		if c.nGPS == 0 {
			return 0, 0
		}
		return c.latSum / float64(c.nGPS), c.lngSum / float64(c.nGPS)
	}

	var clusters []*cluster
	var noGPS []dbgen.Photo

	var cur *cluster
	for _, p := range photos {
		hasGPS := p.Lat != nil && p.Lng != nil

		if !hasGPS {
			// Photos without GPS: try to attach to current cluster (they
			// appeared in time order, so they likely belong here).  If no
			// cluster has started yet, save for later.
			if cur != nil {
				cur.photos = append(cur.photos, p)
			} else {
				noGPS = append(noGPS, p)
			}
			continue
		}

		plat, plng := *p.Lat, *p.Lng

		if cur == nil || cur.nGPS == 0 {
			// First GPS photo or first cluster — start a new cluster.
			if cur == nil {
				cur = &cluster{}
				clusters = append(clusters, cur)
			}
			cur.photos = append(cur.photos, p)
			cur.latSum += plat
			cur.lngSum += plng
			cur.nGPS++
			continue
		}

		clat, clng := centroid(cur)
		d := haversineMeters(plat, plng, clat, clng)

		if d <= radiusMeters {
			// Still in same cluster.
			cur.photos = append(cur.photos, p)
			cur.latSum += plat
			cur.lngSum += plng
			cur.nGPS++
		} else {
			// Too far — start a new cluster.
			cur = &cluster{}
			clusters = append(clusters, cur)
			cur.photos = append(cur.photos, p)
			cur.latSum += plat
			cur.lngSum += plng
			cur.nGPS++
		}
	}

	log.Printf("resort: %d clusters, %d photos without GPS (no cluster)", len(clusters), len(noGPS))

	// ----- Delete old stops, clear assignments -----
	if err := q.DeleteStopsByTrip(ctx, id); err != nil {
		log.Printf("resort: delete stops: %v", err)
	}
	if err := q.ClearPhotoStopIDs(ctx, id); err != nil {
		log.Printf("resort: clear stop ids: %v", err)
	}

	// ----- Create new stops from clusters -----
	now := time.Now()
	for order, c := range clusters {
		if c.nGPS == 0 {
			continue
		}
		stopID := uuid.New().String()

		clat, clng := centroid(c)

		// Earliest taken_at in cluster.
		var arrivedAt *time.Time
		for _, p := range c.photos {
			if p.TakenAt != nil {
				arrivedAt = p.TakenAt
				break
			}
		}

		title := fmt.Sprintf("Stop %d", order+1)

		// Reverse geocode at city level (with rate limit).
		if order > 0 {
			time.Sleep(1100 * time.Millisecond)
		}
		locName := reverseGeocodeCity(clat, clng)

		if err := q.CreateStop(ctx, dbgen.CreateStopParams{
			ID:           stopID,
			TripID:       id,
			Title:        title,
			Lat:          clat,
			Lng:          clng,
			StopOrder:    int64(order),
			ArrivedAt:    arrivedAt,
			CreatedAt:    now,
			LocationName: locName,
		}); err != nil {
			log.Printf("resort: create stop %d: %v", order, err)
			continue
		}

		// Assign photos.
		sortPhotosByTime(c.photos)
		for i, p := range c.photos {
			sid := stopID
			q.SetPhotoStopAndOrder(ctx, dbgen.SetPhotoStopAndOrderParams{
				StopID:     &sid,
				PhotoOrder: int64(i),
				ID:         p.ID,
			})
		}
	}

	// Order unassigned (no-GPS) photos.
	sortPhotosByTime(noGPS)
	for i, p := range noGPS {
		q.SetPhotoStopAndOrder(ctx, dbgen.SetPhotoStopAndOrderParams{
			StopID:     nil,
			PhotoOrder: int64(i),
			ID:         p.ID,
		})
	}

	// ----- Return updated trip detail -----
	updatedTrip, err := q.GetTrip(ctx, id)
	if err != nil {
		log.Printf("resort: re-fetch trip: %v", err)
		jsonError(w, "failed to read trip", http.StatusInternalServerError)
		return
	}
	detail, err := s.buildTripDetail(r, updatedTrip)
	if err != nil {
		log.Printf("resort: build detail: %v", err)
		jsonError(w, "failed to build trip detail", http.StatusInternalServerError)
		return
	}
	jsonOK(w, detail)
}

// sortPhotosByTime sorts photos by taken_at (nulls last), then created_at.
func sortPhotosByTime(photos []dbgen.Photo) {
	sort.Slice(photos, func(i, j int) bool {
		a, b := photos[i], photos[j]
		if a.TakenAt != nil && b.TakenAt != nil {
			if !a.TakenAt.Equal(*b.TakenAt) {
				return a.TakenAt.Before(*b.TakenAt)
			}
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.TakenAt != nil {
			return true
		}
		if b.TakenAt != nil {
			return false
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
}
