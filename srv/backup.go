package srv

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"srv.exe.dev/db/dbgen"
)

// ---------------------------------------------------------------------------
// Backup / Restore (export & import) for trips
// ---------------------------------------------------------------------------

const backupManifestVersion = 1

type backupManifest struct {
	Version    int             `json:"version"`
	ExportedAt time.Time       `json:"exported_at"`
	Trip       dbgen.Trip      `json:"trip"`
	Stops      []dbgen.Stop    `json:"stops"`
	Photos     []dbgen.Photo   `json:"photos"`
	Routes     []dbgen.Route   `json:"routes"`
	Comments   []dbgen.Comment `json:"comments"`
}

// GET /api/trips/{id}/export
func (s *Server) handleExportTrip(w http.ResponseWriter, r *http.Request) {
	tripID := r.PathValue("id")
	trip, _, ok := s.requireTripOwner(w, r, tripID)
	if !ok {
		return
	}

	ctx := r.Context()
	q := dbgen.New(s.DB)

	stops, err := q.ListStops(ctx, tripID)
	if err != nil {
		slog.Error("export: list stops", "err", err)
		jsonError(w, "failed to list stops", http.StatusInternalServerError)
		return
	}
	photos, err := q.ListPhotos(ctx, tripID)
	if err != nil {
		slog.Error("export: list photos", "err", err)
		jsonError(w, "failed to list photos", http.StatusInternalServerError)
		return
	}
	routes, err := q.ListRoutes(ctx, tripID)
	if err != nil {
		slog.Error("export: list routes", "err", err)
		jsonError(w, "failed to list routes", http.StatusInternalServerError)
		return
	}
	comments, err := q.ListCommentsByTrip(ctx, tripID)
	if err != nil {
		slog.Error("export: list comments", "err", err)
		jsonError(w, "failed to list comments", http.StatusInternalServerError)
		return
	}

	manifest := backupManifest{
		Version:    backupManifestVersion,
		ExportedAt: time.Now().UTC(),
		Trip:       trip,
		Stops:      stops,
		Photos:     photos,
		Routes:     routes,
		Comments:   comments,
	}

	// Sanitise the trip title for use as a filename.
	safeTitle := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '"' || r == '\'' || r == '?' || r == '*' || r == '<' || r == '>' || r == '|' {
			return '-'
		}
		return r
	}, trip.Title)
	if safeTitle == "" {
		safeTitle = "trip"
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, safeTitle))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// Write manifest.json
	mf, err := zw.Create("manifest.json")
	if err != nil {
		slog.Error("export: create manifest entry", "err", err)
		return
	}
	enc := json.NewEncoder(mf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		slog.Error("export: encode manifest", "err", err)
		return
	}

	// Write photo/video files and their thumbnails.
	for _, p := range photos {
		// Main file
		mainPath := filepath.Join(s.UploadDir, p.Filename)
		if err := addFileToZip(zw, "files/"+p.Filename, mainPath); err != nil {
			slog.Warn("export: skipping file", "filename", p.Filename, "err", err)
		}

		// Thumbnail (same name in thumbs/)
		thumbPath := filepath.Join(s.UploadDir, "thumbs", p.Filename)
		if err := addFileToZip(zw, "thumbs/"+p.Filename, thumbPath); err != nil {
			// Not all photos may have a same-name thumb; try .jpg variant for videos
			vidThumbPath := filepath.Join(s.UploadDir, "thumbs", p.Filename+".jpg")
			if err2 := addFileToZip(zw, "thumbs/"+p.Filename+".jpg", vidThumbPath); err2 != nil {
				slog.Debug("export: no thumbnail", "filename", p.Filename)
			}
		}
	}

	slog.Info("export: completed", "trip_id", tripID, "photos", len(photos))
}

// addFileToZip adds a single file from disk into the zip archive.
func addFileToZip(zw *zip.Writer, zipPath, diskPath string) error {
	f, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = zipPath
	header.Method = zip.Store

	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, f)
	return err
}

// POST /api/trips/import
func (s *Server) handleImportTrip(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}

	// Limit upload size to 8 GB.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<30)

	// Parse multipart form – keep up to 64 MB in memory, rest on disk.
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		slog.Error("import: parse multipart", "err", err)
		jsonError(w, "failed to parse upload", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "missing 'file' field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// archive/zip needs a ReaderAt, so write to a temp file.
	tmp, err := os.CreateTemp("", "trip-import-*.zip")
	if err != nil {
		slog.Error("import: create temp file", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, file)
	if err != nil {
		slog.Error("import: copy to temp", "err", err)
		jsonError(w, "failed to read upload", http.StatusInternalServerError)
		return
	}

	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		slog.Error("import: open zip", "err", err)
		jsonError(w, "invalid zip file", http.StatusBadRequest)
		return
	}

	// Read manifest.json
	var manifest backupManifest
	manifestFile, err := zr.Open("manifest.json")
	if err != nil {
		jsonError(w, "zip missing manifest.json", http.StatusBadRequest)
		return
	}
	if err := json.NewDecoder(manifestFile).Decode(&manifest); err != nil {
		manifestFile.Close()
		jsonError(w, "invalid manifest.json", http.StatusBadRequest)
		return
	}
	manifestFile.Close()

	if manifest.Version < 1 {
		jsonError(w, "unsupported manifest version", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	q := dbgen.New(s.DB)
	now := time.Now()

	// --- Generate new IDs and build old→new mappings ---
	newTripID := uuid.New().String()
	newShareID := uuid.New().String()

	stopIDMap := make(map[string]string, len(manifest.Stops))
	photoIDMap := make(map[string]string, len(manifest.Photos))
	// Maps old photo filename → new filename (for file copying)
	photoFileMap := make(map[string]string, len(manifest.Photos))

	// Create new trip
	if err := q.CreateTrip(ctx, dbgen.CreateTripParams{
		ID:          newTripID,
		ShareID:     newShareID,
		Title:       manifest.Trip.Title,
		Description: manifest.Trip.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
		UserID:      &u.UserID,
	}); err != nil {
		slog.Error("import: create trip", "err", err)
		jsonError(w, "failed to create trip", http.StatusInternalServerError)
		return
	}

	// Create stops
	for _, st := range manifest.Stops {
		newStopID := uuid.New().String()
		stopIDMap[st.ID] = newStopID

		if err := q.CreateStop(ctx, dbgen.CreateStopParams{
			ID:           newStopID,
			TripID:       newTripID,
			Title:        st.Title,
			Description:  st.Description,
			Lat:          st.Lat,
			Lng:          st.Lng,
			Elevation:    st.Elevation,
			StopOrder:    st.StopOrder,
			ArrivedAt:    st.ArrivedAt,
			CreatedAt:    st.CreatedAt,
			LocationName: st.LocationName,
		}); err != nil {
			slog.Error("import: create stop", "err", err, "old_id", st.ID)
			jsonError(w, "failed to create stop", http.StatusInternalServerError)
			return
		}

		// Restore camera fields via UpdateStop (CreateStop doesn't include cam fields).
		if st.CamLng != nil || st.CamLat != nil || st.CamHeight != nil || st.CamHeading != nil || st.CamPitch != nil {
			if err := q.UpdateStop(ctx, dbgen.UpdateStopParams{
				Title:        st.Title,
				Description:  st.Description,
				Lat:          st.Lat,
				Lng:          st.Lng,
				Elevation:    st.Elevation,
				StopOrder:    st.StopOrder,
				ArrivedAt:    st.ArrivedAt,
				LocationName: st.LocationName,
				CamLng:       st.CamLng,
				CamLat:       st.CamLat,
				CamHeight:    st.CamHeight,
				CamHeading:   st.CamHeading,
				CamPitch:     st.CamPitch,
				ID:           newStopID,
			}); err != nil {
				slog.Error("import: update stop cam fields", "err", err, "old_id", st.ID)
			}
		}
	}

	// Ensure thumbs directory exists.
	thumbDir := filepath.Join(s.UploadDir, "thumbs")
	os.MkdirAll(thumbDir, 0o755)

	// Build a quick lookup for zip entries.
	zipFiles := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		zipFiles[f.Name] = f
	}

	// Create photos
	for _, p := range manifest.Photos {
		newPhotoID := uuid.New().String()
		photoIDMap[p.ID] = newPhotoID

		// Generate a new filename, preserving the extension.
		ext := filepath.Ext(p.Filename)
		newFilename := uuid.New().String() + ext
		photoFileMap[p.Filename] = newFilename

		// Remap stop ID.
		var newStopID *string
		if p.StopID != nil {
			if mapped, ok := stopIDMap[*p.StopID]; ok {
				newStopID = &mapped
			}
		}

		if err := q.CreatePhoto(ctx, dbgen.CreatePhotoParams{
			ID:           newPhotoID,
			TripID:       newTripID,
			StopID:       newStopID,
			Filename:     newFilename,
			OriginalName: p.OriginalName,
			Caption:      p.Caption,
			Lat:          p.Lat,
			Lng:          p.Lng,
			TakenAt:      p.TakenAt,
			Width:        p.Width,
			Height:       p.Height,
			SizeBytes:    p.SizeBytes,
			CreatedAt:    p.CreatedAt,
			IsVideo:      p.IsVideo,
		}); err != nil {
			slog.Error("import: create photo", "err", err, "old_id", p.ID)
			jsonError(w, "failed to create photo", http.StatusInternalServerError)
			return
		}

		// Restore extra fields via UpdatePhoto (cam fields, etc.).
		if p.CamHeading != nil || p.CamPitch != nil || p.CamRange != nil {
			_ = q.UpdatePhoto(ctx, dbgen.UpdatePhotoParams{
				StopID:     newStopID,
				Caption:    p.Caption,
				Lat:        p.Lat,
				Lng:        p.Lng,
				CamHeading: p.CamHeading,
				CamPitch:   p.CamPitch,
				CamRange:   p.CamRange,
				ID:         newPhotoID,
			})
		}

		// Restore photo_order.
		if p.PhotoOrder != 0 {
			_ = q.UpdatePhotoOrder(ctx, dbgen.UpdatePhotoOrderParams{
				PhotoOrder: p.PhotoOrder,
				ID:         newPhotoID,
			})
		}

		// Copy the main file from zip.
		if zf, ok := zipFiles["files/"+p.Filename]; ok {
			if err := extractZipFile(zf, filepath.Join(s.UploadDir, newFilename)); err != nil {
				slog.Warn("import: extract photo file", "err", err, "filename", p.Filename)
			}
		}

		// Copy thumbnail(s) from zip.
		if zf, ok := zipFiles["thumbs/"+p.Filename]; ok {
			if err := extractZipFile(zf, filepath.Join(thumbDir, newFilename)); err != nil {
				slog.Warn("import: extract thumbnail", "err", err, "filename", p.Filename)
			}
		}
		// Video thumbnail variant (filename.jpg)
		if zf, ok := zipFiles["thumbs/"+p.Filename+".jpg"]; ok {
			if err := extractZipFile(zf, filepath.Join(thumbDir, newFilename+".jpg")); err != nil {
				slog.Warn("import: extract video thumbnail", "err", err, "filename", p.Filename)
			}
		}
	}

	// Create routes
	for _, rt := range manifest.Routes {
		newRouteID := uuid.New().String()
		if err := q.CreateRoute(ctx, dbgen.CreateRouteParams{
			ID:        newRouteID,
			TripID:    newTripID,
			Name:      rt.Name,
			Geojson:   rt.Geojson,
			Color:     rt.Color,
			CreatedAt: rt.CreatedAt,
		}); err != nil {
			slog.Error("import: create route", "err", err, "old_id", rt.ID)
			jsonError(w, "failed to create route", http.StatusInternalServerError)
			return
		}
	}

	// Create comments
	for _, c := range manifest.Comments {
		newCommentID := uuid.New().String()
		newPhotoID := c.PhotoID
		if mapped, ok := photoIDMap[c.PhotoID]; ok {
			newPhotoID = mapped
		}
		if err := q.CreateComment(ctx, dbgen.CreateCommentParams{
			ID:        newCommentID,
			PhotoID:   newPhotoID,
			TripID:    newTripID,
			Author:    c.Author,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		}); err != nil {
			slog.Error("import: create comment", "err", err, "old_id", c.ID)
			jsonError(w, "failed to create comment", http.StatusInternalServerError)
			return
		}
	}

	// Remap and set cover_photo_id + camera defaults on the new trip.
	var newCoverPhotoID *string
	if manifest.Trip.CoverPhotoID != nil {
		if mapped, ok := photoIDMap[*manifest.Trip.CoverPhotoID]; ok {
			newCoverPhotoID = &mapped
		}
	}
	if err := q.UpdateTrip(ctx, dbgen.UpdateTripParams{
		Title:             manifest.Trip.Title,
		Description:       manifest.Trip.Description,
		CoverPhotoID:      newCoverPhotoID,
		DefaultCamHeading: manifest.Trip.DefaultCamHeading,
		DefaultCamPitch:   manifest.Trip.DefaultCamPitch,
		DefaultCamRange:   manifest.Trip.DefaultCamRange,
		UpdatedAt:         now,
		ID:                newTripID,
	}); err != nil {
		slog.Error("import: update trip defaults", "err", err)
	}

	// Fetch the final trip and return as tripDetail.
	newTrip, err := q.GetTrip(ctx, newTripID)
	if err != nil {
		slog.Error("import: get created trip", "err", err)
		jsonError(w, "trip created but failed to read back", http.StatusInternalServerError)
		return
	}

	detail, err := s.buildTripDetail(r, newTrip)
	if err != nil {
		slog.Error("import: build trip detail", "err", err)
		jsonError(w, "trip created but failed to load details", http.StatusInternalServerError)
		return
	}

	slog.Info("import: completed", "trip_id", newTripID, "stops", len(manifest.Stops), "photos", len(manifest.Photos))
	jsonCreated(w, detail)
}

// extractZipFile extracts a single file from a zip archive to disk.
func extractZipFile(zf *zip.File, destPath string) error {
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("open zip entry %s: %w", zf.Name, err)
	}
	defer rc.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("copy %s: %w", zf.Name, err)
	}
	return nil
}
