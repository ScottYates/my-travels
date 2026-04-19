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
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rwcarlsen/goexif/exif"
	"srv.exe.dev/db"
	"srv.exe.dev/db/dbgen"
)

type Server struct {
	DB                 *sql.DB
	Hostname           string
	UploadDir          string
	StaticDir          string
	TemplatesDir       string
	GoogleClientID     string
	GoogleClientSecret string
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
func New(dbPath, hostname, googleClientID, googleClientSecret string) (*Server, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(thisFile)

	uploadDir := filepath.Join(baseDir, "..", "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("create upload dir: %w", err)
	}

	srv := &Server{
		Hostname:           hostname,
		UploadDir:          uploadDir,
		TemplatesDir:       filepath.Join(baseDir, "templates"),
		StaticDir:          filepath.Join(baseDir, "static"),
		GoogleClientID:     googleClientID,
		GoogleClientSecret: googleClientSecret,
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

// ---------------------------------------------------------------------------
// Auth helpers
// ---------------------------------------------------------------------------

type authUser struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

const sessionCookieName = "session"

func (s *Server) getUser(r *http.Request) *authUser {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	q := dbgen.New(s.DB)
	sess, err := q.GetSession(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return &authUser{UserID: sess.UserID, Email: sess.Email}
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *authUser {
	u := s.getUser(r)
	if u == nil {
		jsonError(w, "authentication required", http.StatusUnauthorized)
		return nil
	}
	return u
}

// requireTripOwner loads a trip by ID and verifies the current user owns it.
func (s *Server) requireTripOwner(w http.ResponseWriter, r *http.Request, tripID string) (dbgen.Trip, *authUser, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return dbgen.Trip{}, nil, false
	}
	q := dbgen.New(s.DB)
	trip, err := q.GetTrip(r.Context(), tripID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
		} else {
			jsonError(w, "failed to get trip", http.StatusInternalServerError)
		}
		return dbgen.Trip{}, nil, false
	}
	if trip.UserID == nil || *trip.UserID != u.UserID {
		jsonError(w, "forbidden", http.StatusForbidden)
		return dbgen.Trip{}, nil, false
	}
	return trip, u, true
}

// requireStopOwner loads a stop and verifies the current user owns its parent trip.
func (s *Server) requireStopOwner(w http.ResponseWriter, r *http.Request, stopID string) (dbgen.Stop, *authUser, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return dbgen.Stop{}, nil, false
	}
	q := dbgen.New(s.DB)
	stop, err := q.GetStop(r.Context(), stopID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "stop not found", http.StatusNotFound)
		} else {
			jsonError(w, "failed to get stop", http.StatusInternalServerError)
		}
		return dbgen.Stop{}, nil, false
	}
	trip, err := q.GetTrip(r.Context(), stop.TripID)
	if err != nil || trip.UserID == nil || *trip.UserID != u.UserID {
		jsonError(w, "forbidden", http.StatusForbidden)
		return dbgen.Stop{}, nil, false
	}
	return stop, u, true
}

// requirePhotoOwner loads a photo and verifies the current user owns its parent trip.
func (s *Server) requirePhotoOwner(w http.ResponseWriter, r *http.Request, photoID string) (dbgen.Photo, *authUser, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return dbgen.Photo{}, nil, false
	}
	q := dbgen.New(s.DB)
	photo, err := q.GetPhoto(r.Context(), photoID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "photo not found", http.StatusNotFound)
		} else {
			jsonError(w, "failed to get photo", http.StatusInternalServerError)
		}
		return dbgen.Photo{}, nil, false
	}
	trip, err := q.GetTrip(r.Context(), photo.TripID)
	if err != nil || trip.UserID == nil || *trip.UserID != u.UserID {
		jsonError(w, "forbidden", http.StatusForbidden)
		return dbgen.Photo{}, nil, false
	}
	return photo, u, true
}

// Serve starts the HTTP server with all configured routes.
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()

	// SPA routes
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /share/{shareID}", s.handleIndex)
	mux.HandleFunc("GET /present/{shareID}", s.handleIndex)

	// Auth
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /auth/google/login", s.handleGoogleLogin)
	mux.HandleFunc("GET /auth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.StaticDir))))

	// Uploaded photos
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.UploadDir))))

	// Photo thumbnails (128px, generated on demand and cached)
	mux.HandleFunc("GET /thumb/{filename...}", s.handleThumbnail)

	// Trip CRUD
	mux.HandleFunc("GET /api/trips", s.handleListTrips)
	mux.HandleFunc("POST /api/trips", s.handleCreateTrip)
	mux.HandleFunc("GET /api/trips/{id}", s.handleGetTrip)
	mux.HandleFunc("PUT /api/trips/{id}", s.handleUpdateTrip)
	mux.HandleFunc("DELETE /api/trips/{id}", s.handleDeleteTrip)

	// Share (public)
	mux.HandleFunc("GET /api/share/{shareID}", s.handleGetTripByShareID)

	// Present slug
	mux.HandleFunc("PUT /api/trips/{id}/present-slug", s.handleUpdatePresentSlug)
	mux.HandleFunc("GET /api/present/{slug}", s.handleGetTripByPresentSlug)

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

	// Comment CRUD
	mux.HandleFunc("GET /api/trips/{id}/comments", s.handleListCommentsByTrip)
	mux.HandleFunc("GET /api/share/{shareID}/comments", s.handleListCommentsByShare)
	mux.HandleFunc("POST /api/share/{shareID}/photos/{photoID}/comments", s.handleCreateCommentByShare)
	mux.HandleFunc("POST /api/present/{slug}/photos/{photoID}/comments", s.handleCreateCommentByPresent)
	mux.HandleFunc("POST /api/photos/{photoID}/comments", s.handleCreateComment)
	mux.HandleFunc("PUT /api/comments/{id}", s.handleUpdateComment)
	mux.HandleFunc("DELETE /api/comments/{id}", s.handleDeleteComment)

	// Photo rescan EXIF
	mux.HandleFunc("POST /api/photos/{id}/rescan", s.handleRescanPhoto)

	// Photo assignment & reordering
	mux.HandleFunc("POST /api/photos/{id}/assign", s.handleAssignPhoto)
	mux.HandleFunc("POST /api/photos/reorder", s.handleReorderPhotos)

	// Trip reset & auto-stops
	mux.HandleFunc("POST /api/trips/{id}/reset", s.handleResetTrip)
	mux.HandleFunc("POST /api/trips/{id}/auto-stops", s.handleAutoStops)

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

	// Build auth JSON for injection into the SPA
	authInfo := map[string]any{"authenticated": false}
	u := s.getUser(r)
	if u != nil {
		authInfo = map[string]any{
			"authenticated": true,
			"user_id":       u.UserID,
			"email":         u.Email,
		}
	}
	authJSON, _ := json.Marshal(authInfo)

	data := map[string]any{
		"Hostname":       s.Hostname,
		"AuthJSON":       template.JS(authJSON),
		"GoogleClientID": s.GoogleClientID,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		slog.Warn("render index", "error", err)
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := s.getUser(r)
	if u == nil {
		jsonOK(w, map[string]any{"authenticated": false})
		return
	}
	jsonOK(w, map[string]any{
		"authenticated": true,
		"user_id":       u.UserID,
		"email":         u.Email,
	})
}

// getPublicOrigin returns the public-facing origin (scheme + host) from proxy headers.
func getPublicOrigin(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host
}

// handleGoogleLogin redirects the user to Google's OAuth 2.0 authorization endpoint.
func (s *Server) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	if s.GoogleClientID == "" {
		http.Error(w, "Google OAuth not configured", http.StatusInternalServerError)
		return
	}
	origin := getPublicOrigin(r)
	redirectURI := origin + "/auth/google/callback"

	// Generate a random state to prevent CSRF
	state := uuid.New().String()
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   proto(r) == "https",
	})

	params := url.Values{}
	params.Set("client_id", s.GoogleClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", "openid email profile")
	params.Set("state", state)
	params.Set("access_type", "online")
	params.Set("prompt", "select_account")

	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+params.Encode(), http.StatusFound)
}

func proto(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// handleGoogleCallback handles the OAuth 2.0 redirect from Google.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	// Verify state
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return
	}
	// Clear state cookie
	http.SetCookie(w, &http.Cookie{
		Name: "oauth_state", Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	origin := getPublicOrigin(r)
	redirectURI := origin + "/auth/google/callback"

	// Exchange authorization code for tokens
	tokenResp, err := exchangeGoogleCode(r.Context(), code, s.GoogleClientID, s.GoogleClientSecret, redirectURI)
	if err != nil {
		slog.Error("google token exchange", "error", err)
		http.Error(w, "authentication failed", http.StatusInternalServerError)
		return
	}

	// Verify the ID token
	claims, err := verifyGoogleIDToken(r.Context(), tokenResp.IDToken, s.GoogleClientID)
	if err != nil {
		slog.Error("google token verification", "error", err)
		http.Error(w, "authentication failed", http.StatusInternalServerError)
		return
	}

	// Create session
	token := uuid.New().String()
	now := time.Now()
	expires := now.Add(30 * 24 * time.Hour)

	q := dbgen.New(s.DB)
	_ = q.DeleteExpiredSessions(r.Context())

	if err := q.CreateSession(r.Context(), dbgen.CreateSessionParams{
		Token:     token,
		UserID:    claims.Sub,
		Email:     claims.Email,
		CreatedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: expires.UTC().Format(time.RFC3339),
	}); err != nil {
		slog.Error("create session", "error", err)
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   proto(r) == "https",
	})

	// Redirect back to the app
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err == nil && c.Value != "" {
		q := dbgen.New(s.DB)
		_ = q.DeleteSession(r.Context(), c.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	jsonOK(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Trip detail response (shared by get-by-id and get-by-share)
// ---------------------------------------------------------------------------

type tripDetail struct {
	dbgen.Trip
	Stops    []dbgen.Stop    `json:"stops"`
	Photos   []dbgen.Photo   `json:"photos"`
	Routes   []dbgen.Route   `json:"routes"`
	Comments []dbgen.Comment `json:"comments"`
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
	comments, err := q.ListCommentsByTrip(ctx, trip.ID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}

	return &tripDetail{
		Trip:     trip,
		Stops:    stops,
		Photos:   photos,
		Routes:   routes,
		Comments: comments,
	}, nil
}

// ---------------------------------------------------------------------------
// Trips
// ---------------------------------------------------------------------------

func (s *Server) handleListTrips(w http.ResponseWriter, r *http.Request) {
	u := s.getUser(r)
	q := dbgen.New(s.DB)
	var trips []dbgen.Trip
	var err error
	if u != nil {
		// Auto-claim any orphaned trips (from before auth was added)
		_ = q.ClaimOrphanedTrips(r.Context(), &u.UserID)

		trips, err = q.ListTripsByUser(r.Context(), &u.UserID)
	} else {
		// Unauthenticated: show nothing (they should log in)
		trips = []dbgen.Trip{}
	}
	if err != nil {
		jsonError(w, "failed to list trips", http.StatusInternalServerError)
		return
	}
	jsonOK(w, trips)
}

func (s *Server) handleCreateTrip(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}

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
		UserID:      &u.UserID,
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
	trip, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
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
	existing, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
		return
	}

	var body struct {
		Title             string   `json:"title"`
		Description       string   `json:"description"`
		CoverPhotoID      *string  `json:"cover_photo_id"`
		DefaultCamHeading *float64 `json:"default_cam_heading"`
		DefaultCamPitch   *float64 `json:"default_cam_pitch"`
		DefaultCamRange   *float64 `json:"default_cam_range"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	q := dbgen.New(s.DB)

	// Preserve existing camera defaults if not provided in this update
	camH := body.DefaultCamHeading
	if camH == nil {
		camH = existing.DefaultCamHeading
	}
	camP := body.DefaultCamPitch
	if camP == nil {
		camP = existing.DefaultCamPitch
	}
	camR := body.DefaultCamRange
	if camR == nil {
		camR = existing.DefaultCamRange
	}

	if err := q.UpdateTrip(r.Context(), dbgen.UpdateTripParams{
		Title:             body.Title,
		Description:       body.Description,
		CoverPhotoID:      body.CoverPhotoID,
		DefaultCamHeading: camH,
		DefaultCamPitch:   camP,
		DefaultCamRange:   camR,
		UpdatedAt:         time.Now(),
		ID:                id,
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
	if _, _, ok := s.requireTripOwner(w, r, id); !ok {
		return
	}
	q := dbgen.New(s.DB)

	// Delete uploaded photo/video files for this trip
	photos, _ := q.ListPhotos(r.Context(), id)
	for _, p := range photos {
		os.Remove(filepath.Join(s.UploadDir, p.Filename))
		os.Remove(filepath.Join(s.UploadDir, "thumbs", p.Filename))
		os.Remove(filepath.Join(s.UploadDir, "thumbs", p.Filename+".jpg"))
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
// Present Slug
// ---------------------------------------------------------------------------

func (s *Server) handleUpdatePresentSlug(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.requireTripOwner(w, r, id); !ok {
		return
	}

	var body struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Validate slug
	slug := strings.TrimSpace(body.Slug)
	if slug == "" {
		jsonError(w, "slug is required", http.StatusBadRequest)
		return
	}
	if len(slug) > 80 {
		jsonError(w, "slug must be 80 characters or fewer", http.StatusBadRequest)
		return
	}
	// Only allow URL-safe characters
	for _, c := range slug {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			jsonError(w, "slug may only contain letters, numbers, hyphens, and underscores", http.StatusBadRequest)
			return
		}
	}

	q := dbgen.New(s.DB)

	// Check uniqueness (try to find another trip with this slug)
	existing, err := q.GetTripByPresentSlug(r.Context(), &slug)
	if err == nil && existing.ID != id {
		jsonError(w, "this presentation name is already taken", http.StatusConflict)
		return
	}

	if err := q.UpdatePresentSlug(r.Context(), dbgen.UpdatePresentSlugParams{
		PresentSlug: &slug,
		UpdatedAt:   time.Now(),
		ID:          id,
	}); err != nil {
		jsonError(w, "failed to update presentation slug", http.StatusInternalServerError)
		return
	}

	trip, err := q.GetTrip(r.Context(), id)
	if err != nil {
		jsonError(w, "failed to read updated trip", http.StatusInternalServerError)
		return
	}
	jsonOK(w, trip)
}

func (s *Server) handleGetTripByPresentSlug(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	q := dbgen.New(s.DB)

	// Try present_slug first, then fall back to share_id
	trip, err := q.GetTripByPresentSlug(r.Context(), &slug)
	if err != nil {
		if err == sql.ErrNoRows {
			// Fallback: try share_id for backward compat
			trip, err = q.GetTripByShareID(r.Context(), slug)
			if err != nil {
				jsonError(w, "presentation not found", http.StatusNotFound)
				return
			}
		} else {
			jsonError(w, "failed to get trip", http.StatusInternalServerError)
			return
		}
	}

	detail, err := s.buildTripDetail(r, trip)
	if err != nil {
		jsonError(w, "failed to load trip details", http.StatusInternalServerError)
		return
	}
	jsonOK(w, detail)
}

// resolveTrip finds a trip by present_slug first, then share_id as fallback
func (s *Server) resolveTripBySlugOrShareID(r *http.Request, slug string) (dbgen.Trip, error) {
	q := dbgen.New(s.DB)
	trip, err := q.GetTripByPresentSlug(r.Context(), &slug)
	if err == nil {
		return trip, nil
	}
	if err == sql.ErrNoRows {
		return q.GetTripByShareID(r.Context(), slug)
	}
	return trip, err
}

func (s *Server) handleCreateCommentByPresent(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	photoID := r.PathValue("photoID")
	ctx := r.Context()

	trip, err := s.resolveTripBySlugOrShareID(r, slug)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "presentation not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	// Verify photo belongs to the trip
	q := dbgen.New(s.DB)
	photo, err := q.GetPhoto(ctx, photoID)
	if err != nil {
		jsonError(w, "photo not found", http.StatusNotFound)
		return
	}
	if photo.TripID != trip.ID {
		jsonError(w, "photo does not belong to this trip", http.StatusForbidden)
		return
	}

	s.createComment(w, r, trip.ID, photoID)
}

// ---------------------------------------------------------------------------
// Stops
// ---------------------------------------------------------------------------

func (s *Server) handleCreateStop(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")
	if _, _, ok := s.requireTripOwner(w, r, tripID); !ok {
		return
	}

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

	// Verify trip exists (already done by requireTripOwner but need err block structure)
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

	locName := reverseGeocode(body.Lat, body.Lng)

	if err := q.CreateStop(r.Context(), dbgen.CreateStopParams{
		ID:           id,
		TripID:       tripID,
		Title:        body.Title,
		Description:  body.Description,
		Lat:          body.Lat,
		Lng:          body.Lng,
		Elevation:    body.Elevation,
		StopOrder:    nextOrder,
		ArrivedAt:    body.ArrivedAt,
		CreatedAt:    now,
		LocationName: locName,
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
	if _, _, ok := s.requireStopOwner(w, r, id); !ok {
		return
	}

	var body struct {
		Title        string     `json:"title"`
		Description  string     `json:"description"`
		Lat          float64    `json:"lat"`
		Lng          float64    `json:"lng"`
		Elevation    float64    `json:"elevation"`
		StopOrder    int64      `json:"stop_order"`
		ArrivedAt    *time.Time `json:"arrived_at"`
		LocationName *string    `json:"location_name"`
		CamLng       *float64   `json:"cam_lng"`
		CamLat       *float64   `json:"cam_lat"`
		CamHeight    *float64   `json:"cam_height"`
		CamHeading   *float64   `json:"cam_heading"`
		CamPitch     *float64   `json:"cam_pitch"`
		ClearCamera  bool       `json:"clear_camera"`
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

	// Preserve existing optional fields when not provided
	existing, _ := q.GetStop(r.Context(), id)
	locName := existing.LocationName
	if body.LocationName != nil {
		locName = *body.LocationName
	}

	// Preserve existing camera fields if not provided; clear all if clear_camera is set
	var camLng, camLat, camHeight, camHeading, camPitch *float64
	if body.ClearCamera {
		// Explicitly clear all camera fields
	} else if body.CamLng != nil {
		camLng = body.CamLng
		camLat = body.CamLat
		camHeight = body.CamHeight
		camHeading = body.CamHeading
		camPitch = body.CamPitch
	} else {
		camLng = existing.CamLng
		camLat = existing.CamLat
		camHeight = existing.CamHeight
		camHeading = existing.CamHeading
		camPitch = existing.CamPitch
	}

	if err := q.UpdateStop(r.Context(), dbgen.UpdateStopParams{
		Title:        body.Title,
		Description:  body.Description,
		Lat:          body.Lat,
		Lng:          body.Lng,
		Elevation:    body.Elevation,
		StopOrder:    body.StopOrder,
		ArrivedAt:    body.ArrivedAt,
		LocationName: locName,
		CamLng:       camLng,
		CamLat:       camLat,
		CamHeight:    camHeight,
		CamHeading:   camHeading,
		CamPitch:     camPitch,
		ID:           id,
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
	if _, _, ok := s.requireStopOwner(w, r, id); !ok {
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

// isVideoFile checks if a filename has a video extension.
func isVideoFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4", ".mov", ".avi", ".mkv", ".webm", ".m4v", ".3gp":
		return true
	}
	return false
}

// isVideoMIME checks if a MIME type is a video type.
func isVideoMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "video/")
}

// videoThumbnail generates a JPEG thumbnail from a video file using ffmpeg.
// Returns the thumbnail JPEG data, or an error.
func videoThumbnail(videoPath string, maxDim int) ([]byte, error) {
	// Extract a frame at 1 second (or the first frame if shorter)
	cmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-ss", "1",
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxDim, maxDim),
		"-f", "image2",
		"-c:v", "mjpeg",
		"-q:v", "5",
		"pipe:1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Try without -ss (very short videos)
		cmd2 := exec.Command("ffmpeg",
			"-i", videoPath,
			"-vframes", "1",
			"-vf", fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxDim, maxDim),
			"-f", "image2",
			"-c:v", "mjpeg",
			"-q:v", "5",
			"pipe:1",
		)
		stdout.Reset()
		cmd2.Stdout = &stdout
		cmd2.Stderr = &stderr
		if err := cmd2.Run(); err != nil {
			return nil, fmt.Errorf("ffmpeg thumbnail: %w: %s", err, stderr.String())
		}
	}
	return stdout.Bytes(), nil
}

// videoDimensions uses ffprobe to get video width and height.
func videoDimensions(videoPath string) (int, int) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0:s=x",
		videoPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "x")
	if len(parts) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h
}

func (s *Server) handleUploadPhoto(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")
	if _, _, ok := s.requireTripOwner(w, r, tripID); !ok {
		return
	}

	q := dbgen.New(s.DB)

	// Parse multipart form (200 MB max to support video uploads)
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		jsonError(w, "failed to parse multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		jsonError(w, "photo field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read entire file into memory
	fileData, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	// Generate filename preserving extension
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".jpg"
	}
	filename := uuid.New().String() + ext

	// Detect if this is a video
	videoUpload := isVideoFile(header.Filename) || isVideoMIME(header.Header.Get("Content-Type"))

	// Save to upload dir
	dstPath := filepath.Join(s.UploadDir, filename)
	if err := os.WriteFile(dstPath, fileData, 0o644); err != nil {
		jsonError(w, "failed to save file", http.StatusInternalServerError)
		return
	}

	var exifLat, exifLng *float64
	var exifTime *time.Time
	var imgW, imgH int

	if videoUpload {
		// Use ffprobe for video dimensions
		imgW, imgH = videoDimensions(dstPath)
		// Pre-generate video thumbnail
		if thumbData, err := videoThumbnail(dstPath, 128); err == nil {
			thumbDir := filepath.Join(s.UploadDir, "thumbs")
			os.MkdirAll(thumbDir, 0755)
			os.WriteFile(filepath.Join(thumbDir, filename+".jpg"), thumbData, 0644)
		}
	} else {
		// Extract EXIF GPS + timestamp from images
		exifLat, exifLng, exifTime = extractEXIF(fileData)
		// Extract image dimensions
		imgW, imgH = imageDimensions(fileData)
	}

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

	var isVideo int64
	if videoUpload {
		isVideo = 1
	}

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
		IsVideo:      isVideo,
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
	if _, _, ok := s.requirePhotoOwner(w, r, id); !ok {
		return
	}

	var body struct {
		StopID     *string  `json:"stop_id"`
		Caption    string   `json:"caption"`
		Lat        *float64 `json:"lat"`
		Lng        *float64 `json:"lng"`
		CamHeading *float64 `json:"cam_heading"`
		CamPitch   *float64 `json:"cam_pitch"`
		CamRange   *float64 `json:"cam_range"`
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
		StopID:     body.StopID,
		Caption:    body.Caption,
		Lat:        body.Lat,
		Lng:        body.Lng,
		CamHeading: body.CamHeading,
		CamPitch:   body.CamPitch,
		CamRange:   body.CamRange,
		ID:         id,
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
	if _, _, ok := s.requirePhotoOwner(w, r, id); !ok {
		return
	}
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

	// Remove file and thumbnail from disk
	os.Remove(filepath.Join(s.UploadDir, photo.Filename))
	os.Remove(filepath.Join(s.UploadDir, "thumbs", photo.Filename))
	os.Remove(filepath.Join(s.UploadDir, "thumbs", photo.Filename+".jpg")) // video thumb

	if err := q.DeletePhoto(r.Context(), id); err != nil {
		jsonError(w, "failed to delete photo", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Rescan photo EXIF (re-extract GPS + timestamp from file on disk)
// ---------------------------------------------------------------------------

func (s *Server) handleRescanPhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.requirePhotoOwner(w, r, id); !ok {
		return
	}
	ctx := r.Context()
	q := dbgen.New(s.DB)

	photo, err := q.GetPhoto(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "photo not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get photo", http.StatusInternalServerError)
		return
	}

	// Read file from disk
	filePath := filepath.Join(s.UploadDir, photo.Filename)

	// Skip rescan for video files (no EXIF data)
	if photo.IsVideo != 0 {
		updated, err := q.GetPhoto(ctx, id)
		if err != nil {
			jsonError(w, "failed to read photo", http.StatusInternalServerError)
			return
		}
		type rescanResult struct {
			dbgen.Photo
			Found bool `json:"location_found"`
		}
		jsonOK(w, rescanResult{Photo: updated, Found: false})
		return
	}

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		jsonError(w, "photo file not found on disk", http.StatusNotFound)
		return
	}

	exifLat, exifLng, exifTime := extractEXIF(fileData)

	// Update photo with any newly found data
	newLat := photo.Lat
	newLng := photo.Lng
	if exifLat != nil {
		newLat = exifLat
	}
	if exifLng != nil {
		newLng = exifLng
	}

	if err := q.UpdatePhoto(ctx, dbgen.UpdatePhotoParams{
		StopID:     photo.StopID,
		Caption:    photo.Caption,
		Lat:        newLat,
		Lng:        newLng,
		CamHeading: photo.CamHeading,
		CamPitch:   photo.CamPitch,
		CamRange:   photo.CamRange,
		ID:         id,
	}); err != nil {
		jsonError(w, "failed to update photo", http.StatusInternalServerError)
		return
	}

	// Also update taken_at if found (direct SQL since sqlc UpdatePhoto doesn't include it)
	if exifTime != nil {
		s.DB.ExecContext(ctx, "UPDATE photos SET taken_at = ? WHERE id = ?", exifTime, id)
	}

	updated, err := q.GetPhoto(ctx, id)
	if err != nil {
		jsonError(w, "failed to read updated photo", http.StatusInternalServerError)
		return
	}

	found := exifLat != nil && exifLng != nil
	type rescanResult struct {
		dbgen.Photo
		Found bool `json:"location_found"`
	}
	jsonOK(w, rescanResult{Photo: updated, Found: found})
}

// ---------------------------------------------------------------------------
// Photo assignment & reordering
// ---------------------------------------------------------------------------

func (s *Server) handleAssignPhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	photo, _, ok := s.requirePhotoOwner(w, r, id)
	if !ok {
		return
	}

	var body struct {
		StopID *string `json:"stop_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	q := dbgen.New(s.DB)

	// If assigning to a stop, verify the stop belongs to the same trip
	if body.StopID != nil && *body.StopID != "" {
		stop, err := q.GetStop(r.Context(), *body.StopID)
		if err != nil {
			jsonError(w, "stop not found", http.StatusNotFound)
			return
		}
		if stop.TripID != photo.TripID {
			jsonError(w, "stop belongs to a different trip", http.StatusBadRequest)
			return
		}
	}

	newStopID := body.StopID
	if body.StopID != nil && *body.StopID == "" {
		newStopID = nil // unassign
	}

	if err := q.SetPhotoStopID(r.Context(), dbgen.SetPhotoStopIDParams{
		StopID: newStopID,
		ID:     id,
	}); err != nil {
		jsonError(w, "failed to assign photo", http.StatusInternalServerError)
		return
	}

	updated, _ := q.GetPhoto(r.Context(), id)
	jsonOK(w, updated)
}

func (s *Server) handleReorderPhotos(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PhotoIDs []string `json:"photo_ids"`
		StopID   *string  `json:"stop_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.PhotoIDs) == 0 {
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	// Verify ownership of the first photo to get the trip
	firstPhoto, _, ok := s.requirePhotoOwner(w, r, body.PhotoIDs[0])
	if !ok {
		return
	}

	q := dbgen.New(s.DB)

	// Verify stop belongs to same trip if provided
	if body.StopID != nil && *body.StopID != "" {
		stop, err := q.GetStop(r.Context(), *body.StopID)
		if err != nil {
			jsonError(w, "stop not found", http.StatusNotFound)
			return
		}
		if stop.TripID != firstPhoto.TripID {
			jsonError(w, "stop belongs to a different trip", http.StatusBadRequest)
			return
		}
	}

	// Update each photo's order (and stop assignment)
	for i, pid := range body.PhotoIDs {
		stopID := body.StopID
		if stopID != nil && *stopID == "" {
			stopID = nil
		}
		if err := q.SetPhotoStopAndOrder(r.Context(), dbgen.SetPhotoStopAndOrderParams{
			StopID:     stopID,
			PhotoOrder: int64(i),
			ID:         pid,
		}); err != nil {
			jsonError(w, "failed to reorder photo "+pid, http.StatusInternalServerError)
			return
		}
	}

	jsonOK(w, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Thumbnails
// ---------------------------------------------------------------------------

func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	// Sanitize: only allow simple filenames (uuid.ext)
	if strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.NotFound(w, r)
		return
	}

	thumbDir := filepath.Join(s.UploadDir, "thumbs")
	thumbPath := filepath.Join(thumbDir, filename)

	// For video files, thumbnail is stored as filename.jpg
	isVid := isVideoFile(filename)
	if isVid {
		thumbPath = filepath.Join(thumbDir, filename+".jpg")
	}

	// Serve cached thumbnail if it exists
	if info, err := os.Stat(thumbPath); err == nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		http.ServeFile(w, r, thumbPath)
		return
	}

	srcPath := filepath.Join(s.UploadDir, filename)

	if isVid {
		// Generate video thumbnail via ffmpeg
		thumbData, err := videoThumbnail(srcPath, 128)
		if err != nil {
			http.Error(w, "failed to generate video thumbnail", http.StatusInternalServerError)
			return
		}
		os.MkdirAll(thumbDir, 0755)
		os.WriteFile(thumbPath, thumbData, 0644)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Length", strconv.Itoa(len(thumbData)))
		w.Write(thumbData)
		return
	}

	// Generate image thumbnail
	srcFile, err := os.Open(srcPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer srcFile.Close()

	srcImg, _, err := image.Decode(srcFile)
	if err != nil {
		http.Error(w, "failed to decode image", http.StatusInternalServerError)
		return
	}

	// Resize to fit within 128x128 (simple nearest-neighbor via SubImage + draw)
	const maxDim = 128
	bounds := srcImg.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	var tw, th int
	if sw >= sh {
		tw = maxDim
		th = maxDim * sh / sw
		if th < 1 { th = 1 }
	} else {
		th = maxDim
		tw = maxDim * sw / sh
		if tw < 1 { tw = 1 }
	}

	// Use simple box-filter downscaling
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			scx := bounds.Min.X + x*sw/tw
			scy := bounds.Min.Y + y*sh/th
			dst.Set(x, y, srcImg.At(scx, scy))
		}
	}

	// Encode to JPEG
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75}); err != nil {
		http.Error(w, "failed to encode thumbnail", http.StatusInternalServerError)
		return
	}

	// Cache to disk
	os.MkdirAll(thumbDir, 0755)
	os.WriteFile(thumbPath, buf.Bytes(), 0644)

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// Reset trip (delete all data, keep trip shell)
// ---------------------------------------------------------------------------

func (s *Server) handleResetTrip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	trip, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	q := dbgen.New(s.DB)

	// Delete photo/video files from disk
	photos, _ := q.ListPhotos(ctx, trip.ID)
	for _, p := range photos {
		os.Remove(filepath.Join(s.UploadDir, p.Filename))
		os.Remove(filepath.Join(s.UploadDir, "thumbs", p.Filename))
		os.Remove(filepath.Join(s.UploadDir, "thumbs", p.Filename+".jpg"))
	}

	// Delete all child records
	q.DeletePhotosByTrip(ctx, id)
	q.DeleteStopsByTrip(ctx, id)
	q.DeleteRoutesByTrip(ctx, id)

	// Reset trip defaults
	q.ResetTripDefaults(ctx, dbgen.ResetTripDefaultsParams{
		UpdatedAt: time.Now(),
		ID:        id,
	})

	// Return cleaned trip detail
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
}

// ---------------------------------------------------------------------------
// Auto-create stops by clustering photos within 3 miles
// ---------------------------------------------------------------------------

const clusterRadiusMiles = 3.0
const clusterRadiusMeters = clusterRadiusMiles * 1609.344

// reverseGeocode returns a human-readable location name for lat/lng
// using the OpenStreetMap Nominatim API. Returns empty string on failure.
func reverseGeocode(lat, lng float64) string {
	url := fmt.Sprintf(
		"https://nominatim.openstreetmap.org/reverse?lat=%f&lon=%f&format=json&zoom=10&addressdetails=1&accept-language=en",
		lat, lng,
	)
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "MyTravels/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		Address struct {
			City        string `json:"city"`
			Town        string `json:"town"`
			Village     string `json:"village"`
			County      string `json:"county"`
			State       string `json:"state"`
			Country     string `json:"country"`
			CountryCode string `json:"country_code"`
		} `json:"address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	a := result.Address
	locality := a.City
	if locality == "" {
		locality = a.Town
	}
	if locality == "" {
		locality = a.Village
	}
	if locality == "" {
		locality = a.County
	}

	if locality != "" && a.Country != "" {
		return locality + ", " + a.Country
	}
	if a.State != "" && a.Country != "" {
		return a.State + ", " + a.Country
	}
	if a.Country != "" {
		return a.Country
	}
	return ""
}

// haversineMeters returns distance in meters between two lat/lng points.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const R = 6371000.0 // Earth radius in meters
	dLat := (lat2 - lat1) * 3.141592653589793 / 180.0
	dLng := (lng2 - lng1) * 3.141592653589793 / 180.0
	la1 := lat1 * 3.141592653589793 / 180.0
	la2 := lat2 * 3.141592653589793 / 180.0

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(la1)*math.Cos(la2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}

type photoCluster struct {
	photos []dbgen.Photo
	latSum float64
	lngSum float64
}

func (c *photoCluster) centroidLat() float64 { return c.latSum / float64(len(c.photos)) }
func (c *photoCluster) centroidLng() float64 { return c.lngSum / float64(len(c.photos)) }

func (c *photoCluster) addPhoto(p dbgen.Photo) {
	c.photos = append(c.photos, p)
	c.latSum += *p.Lat
	c.lngSum += *p.Lng
}

func (s *Server) handleAutoStops(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	trip, _, ok := s.requireTripOwner(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	q := dbgen.New(s.DB)

	// Remove existing stops and stop assignments
	q.DeleteStopsByTrip(ctx, id)
	q.ClearPhotoStopIDs(ctx, id)

	// Get photos with location, sorted by taken_at
	photos, err := q.ListPhotosWithLocation(ctx, id)
	if err != nil {
		jsonError(w, "failed to list photos", http.StatusInternalServerError)
		return
	}

	if len(photos) == 0 {
		detail, _ := s.buildTripDetail(r, trip)
		jsonOK(w, detail)
		return
	}

	// Cluster photos: assign each photo to nearest existing cluster (by centroid)
	// within radius, or start a new cluster
	var clusters []*photoCluster
	for _, p := range photos {
		plat, plng := *p.Lat, *p.Lng
		bestIdx := -1
		bestDist := clusterRadiusMeters + 1

		for i, c := range clusters {
			d := haversineMeters(plat, plng, c.centroidLat(), c.centroidLng())
			if d < bestDist {
				bestDist = d
				bestIdx = i
			}
		}

		if bestIdx >= 0 && bestDist <= clusterRadiusMeters {
			clusters[bestIdx].addPhoto(p)
		} else {
			c := &photoCluster{}
			c.addPhoto(p)
			clusters = append(clusters, c)
		}
	}

	// Sort clusters by earliest photo timestamp
	sort.Slice(clusters, func(i, j int) bool {
		var ti, tj time.Time
		for _, p := range clusters[i].photos {
			if p.TakenAt != nil {
				ti = *p.TakenAt
				break
			}
		}
		for _, p := range clusters[j].photos {
			if p.TakenAt != nil {
				tj = *p.TakenAt
				break
			}
		}
		return ti.Before(tj)
	})

	now := time.Now()

	// Create stops and assign photos
	for order, c := range clusters {
		stopID := uuid.New().String()

		// Earliest taken_at in cluster
		var arrivedAt *time.Time
		for _, p := range c.photos {
			if p.TakenAt != nil {
				arrivedAt = p.TakenAt
				break
			}
		}

		title := fmt.Sprintf("Stop %d", order+1)

		// Reverse geocode (with rate-limit-friendly delay between calls)
		if order > 0 {
			time.Sleep(1100 * time.Millisecond)
		}
		locName := reverseGeocode(c.centroidLat(), c.centroidLng())

		q.CreateStop(ctx, dbgen.CreateStopParams{
			ID:           stopID,
			TripID:       id,
			Title:        title,
			Lat:          c.centroidLat(),
			Lng:          c.centroidLng(),
			StopOrder:    int64(order),
			ArrivedAt:    arrivedAt,
			CreatedAt:    now,
			LocationName: locName,
		})

		// Assign photos to this stop
		for _, p := range c.photos {
			q.SetPhotoStopID(ctx, dbgen.SetPhotoStopIDParams{
				StopID: &stopID,
				ID:     p.ID,
			})
		}
	}

	// Return updated trip detail
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
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func (s *Server) handleListCommentsByTrip(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")
	if _, _, ok := s.requireTripOwner(w, r, tripID); !ok {
		return
	}
	q := dbgen.New(s.DB)

	comments, err := q.ListCommentsByTrip(r.Context(), tripID)
	if err != nil {
		jsonError(w, "failed to list comments", http.StatusInternalServerError)
		return
	}
	jsonOK(w, comments)
}

func (s *Server) handleListCommentsByShare(w http.ResponseWriter, r *http.Request) {
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

	comments, err := q.ListCommentsByTrip(r.Context(), trip.ID)
	if err != nil {
		jsonError(w, "failed to list comments", http.StatusInternalServerError)
		return
	}
	jsonOK(w, comments)
}

func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	photoID := r.PathValue("photoID")
	if _, _, ok := s.requirePhotoOwner(w, r, photoID); !ok {
		return
	}
	ctx := r.Context()
	q := dbgen.New(s.DB)

	photo, err := q.GetPhoto(ctx, photoID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "photo not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get photo", http.StatusInternalServerError)
		return
	}

	s.createComment(w, r, photo.TripID, photoID)
}

func (s *Server) handleCreateCommentByShare(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("shareID")
	photoID := r.PathValue("photoID")
	ctx := r.Context()
	q := dbgen.New(s.DB)

	trip, err := q.GetTripByShareID(ctx, shareID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "trip not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	// Verify the photo belongs to this trip
	photo, err := q.GetPhoto(ctx, photoID)
	if err != nil {
		if err == sql.ErrNoRows {
			jsonError(w, "photo not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get photo", http.StatusInternalServerError)
		return
	}
	if photo.TripID != trip.ID {
		jsonError(w, "photo does not belong to this trip", http.StatusBadRequest)
		return
	}

	s.createComment(w, r, trip.ID, photoID)
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request, tripID, photoID string) {
	var body struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Validate body is not empty
	body.Body = strings.TrimSpace(body.Body)
	if body.Body == "" {
		jsonError(w, "body is required", http.StatusBadRequest)
		return
	}

	// Default author to Anonymous if empty
	body.Author = strings.TrimSpace(body.Author)
	if body.Author == "" {
		body.Author = "Anonymous"
	}

	// Limit lengths
	if len(body.Author) > 50 {
		body.Author = body.Author[:50]
	}
	if len(body.Body) > 500 {
		body.Body = body.Body[:500]
	}

	id := uuid.New().String()
	now := time.Now()

	q := dbgen.New(s.DB)
	if err := q.CreateComment(r.Context(), dbgen.CreateCommentParams{
		ID:        id,
		PhotoID:   photoID,
		TripID:    tripID,
		Author:    body.Author,
		Body:      body.Body,
		CreatedAt: now,
	}); err != nil {
		jsonError(w, "failed to create comment", http.StatusInternalServerError)
		return
	}

	comment := dbgen.Comment{
		ID:        id,
		PhotoID:   photoID,
		TripID:    tripID,
		Author:    body.Author,
		Body:      body.Body,
		CreatedAt: now,
	}
	jsonCreated(w, comment)
}

func (s *Server) handleUpdateComment(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	// Verify the comment exists and the user owns the trip
	comment, err := q.GetComment(r.Context(), id)
	if err != nil {
		jsonError(w, "comment not found", http.StatusNotFound)
		return
	}
	trip, err := q.GetTrip(r.Context(), comment.TripID)
	if err != nil || trip.UserID == nil || *trip.UserID != u.UserID {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	var body struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.Author = strings.TrimSpace(body.Author)
	body.Body = strings.TrimSpace(body.Body)
	if body.Body == "" {
		jsonError(w, "body is required", http.StatusBadRequest)
		return
	}
	if body.Author == "" {
		body.Author = "Anonymous"
	}
	if len(body.Author) > 50 {
		body.Author = body.Author[:50]
	}
	if len(body.Body) > 500 {
		body.Body = body.Body[:500]
	}

	if err := q.UpdateComment(r.Context(), dbgen.UpdateCommentParams{
		Author: body.Author,
		Body:   body.Body,
		ID:     id,
	}); err != nil {
		jsonError(w, "failed to update comment", http.StatusInternalServerError)
		return
	}

	comment.Author = body.Author
	comment.Body = body.Body
	jsonOK(w, comment)
}

func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	// Verify the comment exists and the user owns the trip
	comment, err := q.GetComment(r.Context(), id)
	if err != nil {
		jsonError(w, "comment not found", http.StatusNotFound)
		return
	}
	trip, err := q.GetTrip(r.Context(), comment.TripID)
	if err != nil || trip.UserID == nil || *trip.UserID != u.UserID {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := q.DeleteComment(r.Context(), id); err != nil {
		jsonError(w, "failed to delete comment", http.StatusInternalServerError)
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
	if _, _, ok := s.requireTripOwner(w, r, tripID); !ok {
		return
	}

	q := dbgen.New(s.DB)

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
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	q := dbgen.New(s.DB)

	if err := q.DeleteRoute(r.Context(), id); err != nil {
		jsonError(w, "failed to delete route", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
