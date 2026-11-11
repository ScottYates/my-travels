package srv

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// uploadTypes is the allowlist of file extensions accepted for upload,
// mapped to the Content-Type they are served with. Anything outside this
// list is rejected at upload time and, if already on disk, served as a
// download instead of being rendered by the browser.
var uploadTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".jpe":  "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".heic": "image/heic",
	".heif": "image/heif",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",

	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".mov":  "video/quicktime",
	".avi":  "video/x-msvideo",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".3gp":  "video/3gpp",
}

// unknownUploadExt is the extension given to imported files whose original
// extension is not in uploadTypes. Such files are kept but only ever served
// as downloads.
const unknownUploadExt = ".bin"

// uploadExt returns the lower-cased extension to store an uploaded file
// under, and whether that extension is allowed. A name with no extension is
// treated as a JPEG, which matches the previous behaviour.
func uploadExt(name string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ".jpg", true
	}
	_, ok := uploadTypes[ext]
	return ext, ok
}

// routeColorRe matches the CSS hex colours the UI uses for routes.
var routeColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

const defaultRouteColor = "#3388ff"

// safeRouteColor returns c if it is a plain hex colour, otherwise the
// default. The colour is written into a style attribute by the frontend.
func safeRouteColor(c string) string {
	if routeColorRe.MatchString(c) {
		return c
	}
	return defaultRouteColor
}

// handleUploadFile serves a single file from the upload directory.
//
// It replaces a bare http.FileServer, which listed the directory (exposing
// every user's files) and chose the Content-Type from the stored extension
// or by sniffing the content. Here:
//   - only a plain file name, optionally under thumbs/, is accepted
//   - directories and in-progress chunk files are never served
//   - the Content-Type comes from the allowlist; anything else is sent as
//     an attachment so the browser will not render it
//
// Access is still by unguessable file name with no login, which is what
// share and presentation links rely on.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	name := strings.TrimPrefix(rel, "thumbs/")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_chunks_") {
		http.NotFound(w, r)
		return
	}

	dir := s.UploadDir
	if rel != name {
		dir = filepath.Join(s.UploadDir, "thumbs")
	}
	f, err := os.Open(filepath.Join(dir, name)) // #nosec G304 -- name is a single path element, checked above.
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ctype, ok := uploadTypes[strings.ToLower(filepath.Ext(name))]; ok {
		w.Header().Set("Content-Type", ctype)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	}
	// ServeContent handles Range requests (needed for video seeking) and
	// does not sniff when Content-Type is already set.
	http.ServeContent(w, r, name, info.ModTime(), f)
}
