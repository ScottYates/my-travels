package srv

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rwcarlsen/goexif/exif"
	"srv.exe.dev/db"
	"srv.exe.dev/db/dbgen"
)

type Server struct {
	DB           *sql.DB
	Hostname     string
	UploadDir    string
	StaticDir    string
	TemplatesDir string
}

// JSON response helpers

type errorResponse struct {
	Error string `json:"error"`
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

func jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonCreated(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(data)
}

// New creates a new Server, initializes the database, and ensures the upload directory exists.
func New(dbPath, hostname string) (*Server, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(thisFile)

	uploadDir := filepath.Join(baseDir, "..", "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("create upload dir: %w", err)
	}

	srv := &Server{
		Hostname:     hostname,
		UploadDir:    uploadDir,
		TemplatesDir: filepath.Join(baseDir, "templates"),
		StaticDir:    filepath.Join(baseDir, "static"),
	}
	if err := srv.setUpDatabase(dbPath); err != nil {
		return nil, err
	}
	return srv, nil
}

func (s *Server) setUpDatabase(dbPath string) error {
	wdb, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open db: %w", err)
	}
	s.DB = wdb
	if err := db.RunMigrations(wdb); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}

// Serve starts the HTTP server with all configured routes.
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()

	// SPA routes
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /share/{shareID}", s.handleIndex)

	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.StaticDir))))

	// Uploaded photos
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.UploadDir))))

	// Trip CRUD
	mux.HandleFunc("GET /api/trips", s.handleListTrips)
	mux.HandleFunc("POST /api/trips", s.handleCreateTrip)
	mux.HandleFunc("GET /api/trips/{id}", s.handleGetTrip)
	mux.HandleFunc("PUT /api/trips/{id}", s.handleUpdateTrip)
	mux.HandleFunc("DELETE /api/trips/{id}", s.handleDeleteTrip)

	// Share (public)
	mux.HandleFunc("GET /api/share/{shareID}", s.handleGetTripByShareID)

	// Stop CRUD
	mux.HandleFunc("POST /api/trips/{id}/stops", s.handleCreateStop)
	mux.HandleFunc("PUT /api/stops/{id}", s.handleUpdateStop)
	mux.HandleFunc("DELETE /api/stops/{id}", s.handleDeleteStop)

	// Photo CRUD
	mux.HandleFunc("POST /api/trips/{id}/photos", s.handleUploadPhoto)
	mux.HandleFunc("PUT /api/photos/{id}", s.handleUpdatePhoto)
	mux.HandleFunc("DELETE /api/photos/{id}", s.handleDeletePhoto)

	// Route CRUD
	mux.HandleFunc("POST /api/trips/{id}/routes", s.handleCreateRoute)
	mux.HandleFunc("DELETE /api/routes/{id}", s.handleDeleteRoute)

	slog.Info("starting server", "addr", addr)
	return http.ListenAndServe(addr, mux)
}

// ---------------------------------------------------------------------------
// SPA handler
// ---------------------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.TemplatesDir, "index.html")
	tmpl, err := template.ParseFiles(path)
	if err != nil {
		// Fallback: try serving a plain file if template parsing fails
		http.ServeFile(w, r, path)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, nil); err != nil {
		slog.Warn("render index", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Trip detail response (shared by get-by-id and get-by-share)
// ---------------------------------------------------------------------------

type tripDetail struct {
	dbgen.Trip
	Stops  []dbgen.Stop  `json:"stops"`
	Photos []dbgen.Photo `json:"photos"`
	Routes []dbgen.Route `json:"routes"`
}

func (s *Server) buildTripDetail(r *http.Request, trip dbgen.Trip) (*tripDetail, error) {
	ctx := r.Context()
	q := dbgen.New(s.DB)

	stops, err := q.ListStops(ctx, trip.ID)
	if err != nil {
		return nil, fmt.Errorf("list stops: %w", err)
	}
	photos, err := q.ListPhotos(ctx, trip.ID)
	if err != nil {
		return nil, fmt.Errorf("list photos: %w", err)
	}
	routes, err := q.ListRoutes(ctx, trip.ID)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}

	return &tripDetail{
		Trip:   trip,
		Stops:  stops,
		Photos: photos,
		Routes: routes,
	}, nil
}

// ---------------------------------------------------------------------------
// Trips
// ---------------------------------------------------------------------------

func (s *Server) handleListTrips(w http.ResponseWriter, r *http.Request) {
	q := dbgen.New(s.DB)
	trips, err := q.ListTrips(r.Context())
	if err != nil {
		jsonError(w, "failed to list trips", http.StatusInternalServerError)
		return
	}
	jsonOK(w, trips)
}

func (s *Server) handleCreateTrip(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}

	now := time.Now()
	id := uuid.New().String()
	shareID := uuid.New().String()

	q := dbgen.New(s.DB)
	if err := q.CreateTrip(r.Context(), dbgen.CreateTripParams{
		ID:          id,
		ShareID:     shareID,
		Title:       body.Title,
		Description: body.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		jsonError(w, "failed to create trip", http.StatusInternalServerError)
		return
	}

	trip, err := q.GetTrip(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read created trip", http.StatusInternalServerError)
		return
	}
	jsonCreated(w, trip)
}

func (s *Server) handleGetTrip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := dbgen.New(s.DB)
	trip, err := q.GetTrip(r.Context(), id)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	detail, err := s.buildTripDetail(r, trip)
	if err != nil {
		jsonError(w, "failed to load trip details", http.StatusInternalServerError)
		return
	}
	jsonOK(w, detail)
}

func (s *Server) handleUpdateTrip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Title        string  `json:"title"`
		Description  string  `json:"description"`
		CoverPhotoID *string `json:"cover_photo_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	q := dbgen.New(s.DB)

	// Verify trip exists
	if _, err := q.GetTrip(r.Context(), id); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	if err := q.UpdateTrip(r.Context(), dbgen.UpdateTripParams{
		Title:        body.Title,
		Description:  body.Description,
		CoverPhotoID: body.CoverPhotoID,
		UpdatedAt:    time.Now(),
		ID:           id,
	}); err != nil {
		jsonError(w, "failed to update trip", http.StatusInternalServerError)
		return
	}

	trip, err := q.GetTrip(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read updated trip", http.StatusInternalServerError)
		return
	}
	jsonOK(w, trip)
}

func (s *Server) handleDeleteTrip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	if _, err := q.GetTrip(r.Context(), id); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	// Delete uploaded photo files for this trip
	photos, _ := q.ListPhotos(r.Context(), id)
	for _, p := range photos {
		os.Remove(filepath.Join(s.UploadDir, p.Filename))
	}

	if err := q.DeleteTrip(r.Context(), id); err != nil {
		jsonError(w, "failed to delete trip", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Share (public)
// ---------------------------------------------------------------------------

func (s *Server) handleGetTripByShareID(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("shareID")
	q := dbgen.New(s.DB)
	trip, err := q.GetTripByShareID(r.Context(), shareID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	detail, err := s.buildTripDetail(r, trip)
	if err != nil {
		jsonError(w, "failed to load trip details", http.StatusInternalServerError)
		return
	}
	jsonOK(w, detail)
}

// ---------------------------------------------------------------------------
// Stops
// ---------------------------------------------------------------------------

func (s *Server) handleCreateStop(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")

	var body struct {
		Title       string     `json:"title"`
		Description string     `json:"description"`
		Lat         float64    `json:"lat"`
		Lng         float64    `json:"lng"`
		Elevation   float64    `json:"elevation"`
		ArrivedAt   *time.Time `json:"arrived_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	q := dbgen.New(s.DB)

	// Verify trip exists
	if _, err := q.GetTrip(r.Context(), tripID); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	// Determine next stop order
	var nextOrder int64
	maxOrder, err := q.MaxStopOrder(r.Context(), tripID)
	if err == nil && maxOrder != nil {
		switch v := maxOrder.(type) {
		case int64:
			nextOrder = v + 1
		case float64:
			nextOrder = int64(v) + 1
		default:
			nextOrder = 0
		}
	}

	id := uuid.New().String()
	now := time.Now()

	if err := q.CreateStop(r.Context(), dbgen.CreateStopParams{
		ID:          id,
		TripID:      tripID,
		Title:       body.Title,
		Description: body.Description,
		Lat:         body.Lat,
		Lng:         body.Lng,
		Elevation:   body.Elevation,
		StopOrder:   nextOrder,
		ArrivedAt:   body.ArrivedAt,
		CreatedAt:   now,
	}); err != nil {
		jsonError(w, "failed to create stop", http.StatusInternalServerError)
		return
	}

	stop, err := q.GetStop(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read created stop", http.StatusInternalServerError)
		return
	}
	jsonCreated(w, stop)
}

func (s *Server) handleUpdateStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Title       string     `json:"title"`
		Description string     `json:"description"`
		Lat         float64    `json:"lat"`
		Lng         float64    `json:"lng"`
		Elevation   float64    `json:"elevation"`
		StopOrder   int64      `json:"stop_order"`
		ArrivedAt   *time.Time `json:"arrived_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	q := dbgen.New(s.DB)

	if _, err := q.GetStop(r.Context(), id); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "stop not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get stop", http.StatusInternalServerError)
		return
	}

	if err := q.UpdateStop(r.Context(), dbgen.UpdateStopParams{
		Title:       body.Title,
		Description: body.Description,
		Lat:         body.Lat,
		Lng:         body.Lng,
		Elevation:   body.Elevation,
		StopOrder:   body.StopOrder,
		ArrivedAt:   body.ArrivedAt,
		ID:          id,
	}); err != nil {
		jsonError(w, "failed to update stop", http.StatusInternalServerError)
		return
	}

	stop, err := q.GetStop(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read updated stop", http.StatusInternalServerError)
		return
	}
	jsonOK(w, stop)
}

func (s *Server) handleDeleteStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	if _, err := q.GetStop(r.Context(), id); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "stop not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get stop", http.StatusInternalServerError)
		return
	}

	if err := q.DeleteStop(r.Context(), id); err != nil {
		jsonError(w, "failed to delete stop", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Photos
// ---------------------------------------------------------------------------

// extractEXIF reads EXIF GPS coordinates and timestamp from image data.
func extractEXIF(data []byte) (lat, lng *float64, takenAt *time.Time) {
	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, nil
	}

	// GPS
	la, lo, err := x.LatLong()
	if err == nil {
		lat = &la
		lng = &lo
	}

	// DateTime
	dt, err := x.DateTime()
	if err == nil {
		takenAt = &dt
	}

	return lat, lng, takenAt
}

// imageDimensions decodes just the image config to get width/height.
func imageDimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func (s *Server) handleUploadPhoto(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")

	q := dbgen.New(s.DB)

	// Verify trip exists
	if _, err := q.GetTrip(r.Context(), tripID); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	// Parse multipart form (32 MB max)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, "failed to parse multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		jsonError(w, "photo field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read entire file into memory for EXIF + dimension parsing
	fileData, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "failed to read photo", http.StatusInternalServerError)
		return
	}

	// Generate filename preserving extension
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".jpg"
	}
	filename := uuid.New().String() + ext

	// Save to upload dir
	dstPath := filepath.Join(s.UploadDir, filename)
	if err := os.WriteFile(dstPath, fileData, 0o644); err != nil {
		jsonError(w, "failed to save photo", http.StatusInternalServerError)
		return
	}

	// Extract EXIF GPS + timestamp
	exifLat, exifLng, exifTime := extractEXIF(fileData)

	// Extract image dimensions
	imgW, imgH := imageDimensions(fileData)

	// Form fields override EXIF if provided
	var lat, lng *float64
	if v := r.FormValue("lat"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			lat = &f
		}
	}
	if v := r.FormValue("lng"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			lng = &f
		}
	}
	// Fall back to EXIF
	if lat == nil {
		lat = exifLat
	}
	if lng == nil {
		lng = exifLng
	}

	caption := r.FormValue("caption")

	var stopID *string
	if v := r.FormValue("stop_id"); v != "" {
		stopID = &v
	}

	var takenAt *time.Time
	if exifTime != nil {
		takenAt = exifTime
	}

	id := uuid.New().String()
	now := time.Now()

	if err := q.CreatePhoto(r.Context(), dbgen.CreatePhotoParams{
		ID:           id,
		TripID:       tripID,
		StopID:       stopID,
		Filename:     filename,
		OriginalName: header.Filename,
		Caption:      caption,
		Lat:          lat,
		Lng:          lng,
		TakenAt:      takenAt,
		Width:        int64(imgW),
		Height:       int64(imgH),
		SizeBytes:    int64(len(fileData)),
		CreatedAt:    now,
	}); err != nil {
		os.Remove(dstPath)
		jsonError(w, "failed to create photo record", http.StatusInternalServerError)
		return
	}

	photo, err := q.GetPhoto(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read created photo", http.StatusInternalServerError)
		return
	}
	jsonCreated(w, photo)
}

func (s *Server) handleUpdatePhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		StopID  *string  `json:"stop_id"`
		Caption string   `json:"caption"`
		Lat     *float64 `json:"lat"`
		Lng     *float64 `json:"lng"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	q := dbgen.New(s.DB)

	if _, err := q.GetPhoto(r.Context(), id); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "photo not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get photo", http.StatusInternalServerError)
		return
	}

	if err := q.UpdatePhoto(r.Context(), dbgen.UpdatePhotoParams{
		StopID:  body.StopID,
		Caption: body.Caption,
		Lat:     body.Lat,
		Lng:     body.Lng,
		ID:      id,
	}); err != nil {
		jsonError(w, "failed to update photo", http.StatusInternalServerError)
		return
	}

	photo, err := q.GetPhoto(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read updated photo", http.StatusInternalServerError)
		return
	}
	jsonOK(w, photo)
}

func (s *Server) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	photo, err := q.GetPhoto(r.Context(), id)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "photo not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get photo", http.StatusInternalServerError)
		return
	}

	// Remove file from disk
	os.Remove(filepath.Join(s.UploadDir, photo.Filename))

	if err := q.DeletePhoto(r.Context(), id); err != nil {
		jsonError(w, "failed to delete photo", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

// GPX XML structures for parsing
type gpxFile struct {
	XMLName xml.Name `xml:"gpx"`
	Tracks  []gpxTrk `xml:"trk"`
}

type gpxTrk struct {
	Name     string      `xml:"name"`
	Segments []gpxTrkSeg `xml:"trkseg"`
}

type gpxTrkSeg struct {
	Points []gpxTrkPt `xml:"trkpt"`
}

type gpxTrkPt struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
	Ele float64 `xml:"ele"`
}

// GeoJSON types for building route output
type geoJSONFeatureCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
}

type geoJSONFeature struct {
	Type       string          `json:"type"`
	Properties map[string]any  `json:"properties"`
	Geometry   geoJSONGeometry `json:"geometry"`
}

type geoJSONGeometry struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"`
}

func gpxToGeoJSON(data []byte) (string, string, error) {
	var gpx gpxFile
	if err := xml.Unmarshal(data, &gpx); err != nil {
		return "", "", fmt.Errorf("parse GPX: %w", err)
	}

	var coords [][]float64
	var name string

	for _, trk := range gpx.Tracks {
		if name == "" && trk.Name != "" {
			name = trk.Name
		}
		for _, seg := range trk.Segments {
			for _, pt := range seg.Points {
				coord := []float64{pt.Lon, pt.Lat}
				if pt.Ele != 0 {
					coord = append(coord, pt.Ele)
				}
				coords = append(coords, coord)
			}
		}
	}

	if len(coords) == 0 {
		return "", "", fmt.Errorf("no track points found in GPX")
	}

	if name == "" {
		name = "Imported Route"
	}

	fc := geoJSONFeatureCollection{
		Type: "FeatureCollection",
		Features: []geoJSONFeature{
			{
				Type:       "Feature",
				Properties: map[string]any{"name": name},
				Geometry: geoJSONGeometry{
					Type:        "LineString",
					Coordinates: coords,
				},
			},
		},
	}

	geojsonBytes, err := json.Marshal(fc)
	if err != nil {
		return "", "", fmt.Errorf("marshal GeoJSON: %w", err)
	}

	return string(geojsonBytes), name, nil
}

func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")

	q := dbgen.New(s.DB)

	// Verify trip exists
	if _, err := q.GetTrip(r.Context(), tripID); err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	contentType := r.Header.Get("Content-Type")

	var name, geojson, color string

	if strings.HasPrefix(contentType, "multipart/") {
		// GPX file upload
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jsonError(w, "failed to parse multipart form", http.StatusBadRequest)
			return
		}

		file, _, err := r.FormFile("gpx")
		if err != nil {
			jsonError(w, "gpx field is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		gpxData, err := io.ReadAll(file)
		if err != nil {
			jsonError(w, "failed to read GPX file", http.StatusInternalServerError)
			return
		}

		parsedGeoJSON, parsedName, err := gpxToGeoJSON(gpxData)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}

		geojson = parsedGeoJSON
		name = parsedName
		color = r.FormValue("color")
		if formName := r.FormValue("name"); formName != "" {
			name = formName
		}
	} else {
		// JSON body
		var body struct {
			Name    string `json:"name"`
			Geojson string `json:"geojson"`
			Color   string `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		name = body.Name
		geojson = body.Geojson
		color = body.Color
	}

	if name == "" {
		name = "Route"
	}
	if color == "" {
		color = "#3388ff"
	}
	if geojson == "" {
		jsonError(w, "geojson is required", http.StatusBadRequest)
		return
	}

	id := uuid.New().String()
	now := time.Now()

	if err := q.CreateRoute(r.Context(), dbgen.CreateRouteParams{
		ID:        id,
		TripID:    tripID,
		Name:      name,
		Geojson:   geojson,
		Color:     color,
		CreatedAt: now,
	}); err != nil {
		jsonError(w, "failed to create route", http.StatusInternalServerError)
		return
	}

	route := dbgen.Route{
		ID:        id,
		TripID:    tripID,
		Name:      name,
		Geojson:   geojson,
		Color:     color,
		CreatedAt: now,
	}
	jsonCreated(w, route)
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	if err := q.DeleteRoute(r.Context(), id); err != nil {
		jsonError(w, "failed to delete route", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
