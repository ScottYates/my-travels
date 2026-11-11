package srv

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// orphanMinAge is how old a file must be before the cleanup will delete
// it. An upload writes its file to disk before the photo row exists, and
// chunked uploads and imports can run for a long time, so a file with no
// row is not necessarily abandoned. The modification time moves forward
// with every chunk written, so an upload still in progress stays "young".
const orphanMinAge = 1 * time.Hour

// photoFilenames returns the set of filenames referenced by photo rows.
// Any error is returned: callers decide what is an orphan from this set,
// and a partial set would make real photos look like orphans.
func (s *Server) photoFilenames(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT filename FROM photos")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	files := make(map[string]bool)
	for rows.Next() {
		var fn string
		if err := rows.Scan(&fn); err != nil {
			return nil, err
		}
		files[fn] = true
	}
	return files, rows.Err()
}

// POST /api/admin/orphan-files/delete
// Body: {"files": ["<name>", ...]}
//
// Deletes orphan files: files in the upload directory that no photo row
// refers to. The caller sends the names it wants removed (the list the
// admin page showed), so nothing is deleted that the admin did not see.
// Each name is checked again here and is skipped unless it is:
//   - a plain file name directly inside the upload directory
//   - still not referenced by any photo row
//   - a regular file last modified more than orphanMinAge ago
//
// Thumbnails belonging to a deleted file are removed with it.
func (s *Server) handleAdminDeleteOrphanFiles(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	var body struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	dbFiles, err := s.photoFilenames(r.Context())
	if err != nil {
		slog.Error("admin: delete orphans: list photo filenames", "error", err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	// With no photo rows at all, every file on disk looks like an orphan.
	// That is what a server started against the wrong or a new database
	// looks like, so refuse instead of emptying the upload directory.
	if len(dbFiles) == 0 {
		jsonError(w, "the database has no photos; refusing to delete files because the server may be using the wrong database", http.StatusConflict)
		return
	}

	cutoff := time.Now().Add(-orphanMinAge)
	thumbDir := filepath.Join(s.UploadDir, "thumbs")

	var deleted, skippedReferenced, skippedRecent, skippedOther, failed int
	var freed int64

	for _, name := range body.Files {
		if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
			skippedOther++
			continue
		}
		if dbFiles[name] {
			skippedReferenced++
			continue
		}
		path := filepath.Join(s.UploadDir, name) // #nosec G304 -- name is a single path element, checked above.
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			skippedOther++
			continue
		}
		if info.ModTime().After(cutoff) {
			skippedRecent++
			continue
		}
		if err := os.Remove(path); err != nil {
			slog.Warn("admin: delete orphan file", "file", name, "error", err)
			failed++
			continue
		}
		deleted++
		freed += info.Size()

		// Thumbnails for this file, if any.
		for _, t := range []string{filepath.Join(thumbDir, name), filepath.Join(thumbDir, name+".jpg")} {
			if ti, err := os.Lstat(t); err == nil && ti.Mode().IsRegular() {
				if os.Remove(t) == nil {
					freed += ti.Size()
				}
			}
		}
	}

	slog.Info("admin: deleted orphan files",
		"admin", s.AdminEmail,
		"requested", len(body.Files),
		"deleted", deleted,
		"freed_bytes", freed,
		"skipped_referenced", skippedReferenced,
		"skipped_recent", skippedRecent,
		"skipped_other", skippedOther,
		"failed", failed,
	)

	jsonOK(w, map[string]any{
		"deleted":            deleted,
		"freed_bytes":        freed,
		"freed_human":        humanBytes(freed),
		"skipped_referenced": skippedReferenced,
		"skipped_recent":     skippedRecent,
		"skipped_other":      skippedOther,
		"failed":             failed,
	})
}
