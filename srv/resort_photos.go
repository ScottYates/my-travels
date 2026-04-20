package srv

import (
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"srv.exe.dev/db/dbgen"
)

// ---------------------------------------------------------------------------
// Resort Photos Into Stops
// ---------------------------------------------------------------------------
//
// Assigns (or reassigns) every photo in a trip to the most appropriate stop
// using a combination of GPS proximity and time proximity.
//
// Algorithm:
//   1. Fetch all stops (sorted by stop_order, i.e. chronological by arrival)
//      and all photos (sorted by taken_at) for the trip.
//   2. For each photo, determine the best stop:
//      a) If the photo has GPS coordinates, compute haversine distance to
//         every stop. If the nearest stop is within 50 km, assign to it.
//         If no stop is within 50 km, fall through to time-based assignment.
//      b) If the photo has a taken_at timestamp (and was not GPS-assigned),
//         assign it to the stop whose time window contains taken_at.
//         A stop's time window runs from its arrived_at to the next stop's
//         arrived_at. The last stop's window extends +24 h.
//      c) Photos with neither GPS nor taken_at are left unassigned.
//   3. After assignment, set photo_order per stop (ordered by taken_at then
//      created_at) and photo_order for unassigned photos.
// ---------------------------------------------------------------------------

const resortMaxGPSDistanceMeters = 50_000 // 50 km

func (s *Server) handleResortPhotos(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	trip, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
		return
	}

	ctx := r.Context()
	q := dbgen.New(s.DB)

	// 1. Fetch stops and photos.
	stops, err := q.ListStops(ctx, trip.ID)
	if err != nil {
		log.Printf("resort: failed to list stops for trip %s: %v", trip.ID, err)
		jsonError(w, "failed to list stops", http.StatusInternalServerError)
		return
	}
	photos, err := q.ListPhotos(ctx, trip.ID)
	if err != nil {
		log.Printf("resort: failed to list photos for trip %s: %v", trip.ID, err)
		jsonError(w, "failed to list photos", http.StatusInternalServerError)
		return
	}

	log.Printf("resort: trip %s — %d stops, %d photos", trip.ID, len(stops), len(photos))

	if len(stops) == 0 {
		// No stops to assign to; just order unassigned photos.
		sortPhotosByTime(photos)
		for i, p := range photos {
			if err := q.SetPhotoStopAndOrder(ctx, dbgen.SetPhotoStopAndOrderParams{
				StopID:     nil,
				PhotoOrder: int64(i),
				ID:         p.ID,
			}); err != nil {
				log.Printf("resort: failed to update photo %s: %v", p.ID, err)
			}
		}
		updatedTrip, err := q.GetTrip(ctx, id)
		if err != nil {
			jsonError(w, "failed to read trip", http.StatusInternalServerError)
			return
		}
		detail, err := s.buildTripDetail(r, updatedTrip)
		if err != nil {
			jsonError(w, "failed to build trip detail", http.StatusInternalServerError)
			return
		}
		jsonOK(w, detail)
		return
	}

	// Sort photos by taken_at for consistent processing.
	sortPhotosByTime(photos)

	// 2. Build stop time windows.
	// Each stop owns the interval [arrived_at, next_stop.arrived_at).
	// The last stop's window extends +24h from its arrived_at.
	type stopWindow struct {
		stop  dbgen.Stop
		start time.Time
		end   time.Time
	}
	var windows []stopWindow
	for i, st := range stops {
		if st.ArrivedAt == nil {
			continue
		}
		w := stopWindow{stop: st, start: *st.ArrivedAt}
		// Find the next stop that has an arrived_at.
		found := false
		for j := i + 1; j < len(stops); j++ {
			if stops[j].ArrivedAt != nil {
				w.end = *stops[j].ArrivedAt
				found = true
				break
			}
		}
		if !found {
			w.end = st.ArrivedAt.Add(24 * time.Hour)
		}
		windows = append(windows, w)
	}

	// 3. Assign each photo to the best stop.
	// stopPhotos maps stop ID -> list of photos assigned to it.
	stopPhotos := make(map[string][]dbgen.Photo)
	var unassigned []dbgen.Photo

	var assignedGPS, assignedTime, leftUnassigned int

	for _, p := range photos {
		hasGPS := p.Lat != nil && p.Lng != nil
		hasTime := p.TakenAt != nil

		assignedByGPS := false

		// --- GPS-based assignment ---
		if hasGPS {
			bestDist := math.MaxFloat64
			bestStopID := ""
			for _, st := range stops {
				d := haversineMeters(*p.Lat, *p.Lng, st.Lat, st.Lng)
				if d < bestDist {
					bestDist = d
					bestStopID = st.ID
				}
			}
			if bestDist <= resortMaxGPSDistanceMeters {
				stopPhotos[bestStopID] = append(stopPhotos[bestStopID], p)
				assignedByGPS = true
				assignedGPS++
			}
		}

		// --- Time-based assignment (fallback for GPS too-far or no GPS) ---
		if !assignedByGPS && hasTime {
			t := *p.TakenAt
			assignedByTime := false

			// Check each time window.
			for _, w := range windows {
				if (t.Equal(w.start) || t.After(w.start)) && t.Before(w.end) {
					stopPhotos[w.stop.ID] = append(stopPhotos[w.stop.ID], p)
					assignedByTime = true
					assignedTime++
					break
				}
			}

			// If photo time is before the first window or after the last,
			// assign to the nearest stop by time.
			if !assignedByTime && len(windows) > 0 {
				bestDur := time.Duration(math.MaxInt64)
				bestStopID := ""
				for _, w := range windows {
					d := absDuration(t.Sub(w.start))
					if d < bestDur {
						bestDur = d
						bestStopID = w.stop.ID
					}
				}
				if bestStopID != "" {
					stopPhotos[bestStopID] = append(stopPhotos[bestStopID], p)
					assignedTime++
				} else {
					unassigned = append(unassigned, p)
					leftUnassigned++
				}
			} else if !assignedByTime {
				unassigned = append(unassigned, p)
				leftUnassigned++
			}
			continue
		}

		// No GPS assignment and no time — leave unassigned.
		if !assignedByGPS {
			unassigned = append(unassigned, p)
			leftUnassigned++
		}
	}

	log.Printf("resort: assigned %d by GPS, %d by time, %d left unassigned",
		assignedGPS, assignedTime, leftUnassigned)

	// 4. Persist assignments with photo_order.
	// For each stop, sort its photos by taken_at then created_at.
	for stopID, sp := range stopPhotos {
		sortPhotosByTime(sp)
		sid := stopID // capture for pointer
		for i, p := range sp {
			if err := q.SetPhotoStopAndOrder(ctx, dbgen.SetPhotoStopAndOrderParams{
				StopID:     &sid,
				PhotoOrder: int64(i),
				ID:         p.ID,
			}); err != nil {
				log.Printf("resort: failed to update photo %s: %v", p.ID, err)
			}
		}
	}

	// Order unassigned photos too.
	sortPhotosByTime(unassigned)
	for i, p := range unassigned {
		if err := q.SetPhotoStopAndOrder(ctx, dbgen.SetPhotoStopAndOrderParams{
			StopID:     nil,
			PhotoOrder: int64(i),
			ID:         p.ID,
		}); err != nil {
			log.Printf("resort: failed to update unassigned photo %s: %v", p.ID, err)
		}
	}

	// 5. Re-fetch trip and return full detail.
	updatedTrip, err := q.GetTrip(ctx, id)
	if err != nil {
		log.Printf("resort: failed to re-fetch trip %s: %v", id, err)
		jsonError(w, "failed to read trip", http.StatusInternalServerError)
		return
	}
	detail, err := s.buildTripDetail(r, updatedTrip)
	if err != nil {
		log.Printf("resort: failed to build trip detail for %s: %v", id, err)
		jsonError(w, "failed to build trip detail", http.StatusInternalServerError)
		return
	}
	jsonOK(w, detail)
}

// sortPhotosByTime sorts photos by taken_at (nulls last), then created_at.
func sortPhotosByTime(photos []dbgen.Photo) {
	sort.Slice(photos, func(i, j int) bool {
		a, b := photos[i], photos[j]
		// Both have taken_at: compare them.
		if a.TakenAt != nil && b.TakenAt != nil {
			if !a.TakenAt.Equal(*b.TakenAt) {
				return a.TakenAt.Before(*b.TakenAt)
			}
			return a.CreatedAt.Before(b.CreatedAt)
		}
		// Only one has taken_at: it comes first.
		if a.TakenAt != nil {
			return true
		}
		if b.TakenAt != nil {
			return false
		}
		// Neither: fall back to created_at.
		return a.CreatedAt.Before(b.CreatedAt)
	})
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
