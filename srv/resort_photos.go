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
// Resort Photos Into Stops (with SSE progress)
// ---------------------------------------------------------------------------

const defaultResortRadiusKm = 10.0

// sendSSE writes one Server-Sent Event and flushes.
func sendSSE(w http.ResponseWriter, f http.Flusher, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
	f.Flush()
}

func (s *Server) handleResortPhotos(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	trip, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
		return
	}

	var body struct {
		RadiusKm *float64 `json:"radius_km"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	radiusKm := defaultResortRadiusKm
	if body.RadiusKm != nil && *body.RadiusKm > 0 {
		radiusKm = *body.RadiusKm
	}
	radiusMeters := radiusKm * 1000.0

	ctx := r.Context()
	q := dbgen.New(s.DB)

	photos, err := q.ListPhotos(ctx, trip.ID)
	if err != nil {
		log.Printf("resort: list photos: %v", err)
		jsonError(w, "failed to list photos", http.StatusInternalServerError)
		return
	}

	log.Printf("resort: trip %s — %d photos, radius %.1f km", trip.ID, len(photos), radiusKm)

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
			if cur != nil {
				cur.photos = append(cur.photos, p)
			} else {
				noGPS = append(noGPS, p)
			}
			continue
		}

		plat, plng := *p.Lat, *p.Lng

		if cur == nil || cur.nGPS == 0 {
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
			cur.photos = append(cur.photos, p)
			cur.latSum += plat
			cur.lngSum += plng
			cur.nGPS++
		} else {
			cur = &cluster{}
			clusters = append(clusters, cur)
			cur.photos = append(cur.photos, p)
			cur.latSum += plat
			cur.lngSum += plng
			cur.nGPS++
		}
	}

	log.Printf("resort: %d clusters, %d photos without GPS (no cluster)", len(clusters), len(noGPS))

	// Switch to SSE mode if the client supports it
	flusher, canSSE := w.(http.Flusher)
	if canSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}

	// Count real clusters (with GPS)
	nReal := 0
	for _, c := range clusters {
		if c.nGPS > 0 {
			nReal++
		}
	}

	if canSSE {
		sendSSE(w, flusher, map[string]interface{}{
			"step":  "clustered",
			"total": nReal,
		})
	}

	// ----- Delete old stops, clear assignments -----
	if err := q.DeleteStopsByTrip(ctx, id); err != nil {
		log.Printf("resort: delete stops: %v", err)
	}
	if err := q.ClearPhotoStopIDs(ctx, id); err != nil {
		log.Printf("resort: clear stop ids: %v", err)
	}

	// ----- Create new stops from clusters -----
	now := time.Now()
	geoIdx := 0
	for order, c := range clusters {
		if c.nGPS == 0 {
			continue
		}
		stopID := uuid.New().String()

		clat, clng := centroid(c)

		var arrivedAt *time.Time
		for _, p := range c.photos {
			if p.TakenAt != nil {
				arrivedAt = p.TakenAt
				break
			}
		}

		title := fmt.Sprintf("Stop %d", order+1)

		if order > 0 {
			time.Sleep(1100 * time.Millisecond)
		}

		geoIdx++
		if canSSE {
			sendSSE(w, flusher, map[string]interface{}{
				"step":    "geocoding",
				"current": geoIdx,
				"total":   nReal,
			})
		}

		locName := reverseGeocodeCity(clat, clng)

		if canSSE {
			sendSSE(w, flusher, map[string]interface{}{
				"step":      "geocoded",
				"current":   geoIdx,
				"total":     nReal,
				"stop_name": locName,
			})
		}

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
		if canSSE {
			sendSSE(w, flusher, map[string]string{"step": "error", "message": "failed to read trip"})
		} else {
			jsonError(w, "failed to read trip", http.StatusInternalServerError)
		}
		return
	}
	detail, err := s.buildTripDetail(r, updatedTrip)
	if err != nil {
		log.Printf("resort: build detail: %v", err)
		if canSSE {
			sendSSE(w, flusher, map[string]string{"step": "error", "message": "failed to build trip detail"})
		} else {
			jsonError(w, "failed to build trip detail", http.StatusInternalServerError)
		}
		return
	}

	if canSSE {
		sendSSE(w, flusher, map[string]interface{}{
			"step": "done",
			"trip": detail,
		})
	} else {
		jsonOK(w, detail)
	}
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
