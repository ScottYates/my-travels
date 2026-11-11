package srv

import (
	"bytes"
	"image"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rwcarlsen/goexif/exif"
)

// Size limits. Every request body is capped by one of these.
const (
	// maxPhotoUploadBytes caps a single (non-chunked) photo/video upload.
	maxPhotoUploadBytes = 200 << 20
	// maxChunkBytes caps one chunk of a chunked upload. The client sends
	// 4 MB chunks.
	maxChunkBytes = 16 << 20
	// maxChunkedUploadBytes caps the assembled size of a chunked upload.
	maxChunkedUploadBytes = 8 << 30
	// maxPendingUploadsPerUser caps in-progress chunked uploads per user.
	// The client runs 10 at a time and may leave a few behind on retry.
	maxPendingUploadsPerUser = 100
	// maxImportBytes caps both the uploaded backup zip and the total size
	// of the files it unpacks to.
	maxImportBytes = 8 << 30
	// maxManifestBytes caps manifest.json inside a backup zip.
	maxManifestBytes = 64 << 20
	// maxRouteBodyBytes caps a route upload (GPX file or GeoJSON body).
	maxRouteBodyBytes = 40 << 20
	// maxJSONBodyBytes caps every other request body.
	maxJSONBodyBytes = 1 << 20
	// maxCommentBodyBytes caps comment requests, which need no login.
	maxCommentBodyBytes = 16 << 10
	// maxThumbPixels is the largest image the thumbnailer will decode.
	// It allows 200-megapixel phone photos and rejects decompression bombs.
	maxThumbPixels = 256 << 20
	// maxEXIFBytes is how much of a file the EXIF parser may read.
	maxEXIFBytes = 64 << 20
)

// setsOwnBodyLimit reports whether the handler for this path applies its
// own, larger body limit. MaxBytesReader limits cannot be raised once set,
// so limitRequestBody must leave these alone.
func setsOwnBodyLimit(path string) bool {
	switch {
	case path == "/api/trips/import":
		return true
	case strings.HasPrefix(path, "/api/trips/") &&
		(strings.HasSuffix(path, "/photos") || strings.HasSuffix(path, "/routes")):
		return true
	case strings.HasPrefix(path, "/api/uploads/") && strings.HasSuffix(path, "/chunk"):
		return true
	}
	return false
}

// limitRequestBody caps request bodies for every endpoint that does not
// set its own limit. Without it a client could post an arbitrarily large
// JSON body and have it buffered in memory.
func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody && !setsOwnBodyLimit(r.URL.Path) {
			limit := int64(maxJSONBodyBytes)
			if strings.HasSuffix(r.URL.Path, "/comments") {
				limit = maxCommentBodyBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets response headers that limit what a browser will do
// with our pages.
//
// The Content-Security-Policy here does not restrict script sources: the
// SPA is one large inline script with inline event handlers, and Cesium
// needs workers and eval. It does block plugins, <base> hijacking and
// cross-site form posts.
//
// Framing is limited to same-origin except for /share/ and /present/
// pages, which stay embeddable on other sites as they were before.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

		csp := "object-src 'none'; base-uri 'self'; form-action 'self'"
		public := strings.HasPrefix(r.URL.Path, "/share/") || strings.HasPrefix(r.URL.Path, "/present/")
		if !public {
			csp += "; frame-ancestors 'self'"
			h.Set("X-Frame-Options", "SAMEORIGIN")
		}
		h.Set("Content-Security-Policy", csp)

		if proto(r) == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// extractEXIFFrom reads EXIF GPS coordinates and timestamp from r. Only
// the first maxEXIFBytes are read: the parser buffers TIFF input in memory.
func extractEXIFFrom(r io.Reader) (lat, lng *float64, takenAt *time.Time) {
	// goexif is unmaintained and has panicked on malformed input. A bad
	// file should upload without metadata, not fail the request.
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("exif: parser panicked", "panic", p)
			lat, lng, takenAt = nil, nil, nil
		}
	}()

	x, err := exif.Decode(io.LimitReader(r, maxEXIFBytes))
	if err != nil {
		return nil, nil, nil
	}
	if la, lo, err := x.LatLong(); err == nil {
		lat, lng = &la, &lo
	}
	if dt, err := x.DateTime(); err == nil {
		takenAt = &dt
	}
	return lat, lng, takenAt
}

// extractEXIF reads EXIF GPS coordinates and timestamp from image data.
func extractEXIF(data []byte) (lat, lng *float64, takenAt *time.Time) {
	return extractEXIFFrom(bytes.NewReader(data))
}

// imageFileMeta reads EXIF data and pixel dimensions from an image on disk
// without loading the whole file.
func imageFileMeta(path string) (lat, lng *float64, takenAt *time.Time, width, height int) {
	f, err := os.Open(path) // #nosec G304 -- path is built by the caller from the upload dir and a server-generated name.
	if err != nil {
		return
	}
	defer f.Close()

	lat, lng, takenAt = extractEXIFFrom(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	if cfg, _, err := image.DecodeConfig(f); err == nil {
		width, height = cfg.Width, cfg.Height
	}
	return
}

// uploadMeta gathers what the photo record needs from a file that has
// just been stored at dstPath. For videos it also writes the thumbnail.
func (s *Server) uploadMeta(dstPath, filename string, video bool) (lat, lng *float64, takenAt *time.Time, width, height int) {
	if !video {
		return imageFileMeta(dstPath)
	}
	width, height = videoDimensions(dstPath)
	if thumbData, err := videoThumbnail(dstPath, 128); err == nil {
		thumbDir := filepath.Join(s.UploadDir, "thumbs")
		os.MkdirAll(thumbDir, 0o755)                                             // #nosec G301 -- server-controlled dir.
		os.WriteFile(filepath.Join(thumbDir, filename+".jpg"), thumbData, 0o644) // #nosec G306 -- thumbnails are public by design.
	}
	return
}

// Comment field limits, in bytes.
const (
	maxCommentAuthorLen = 50
	maxCommentBodyLen   = 500
)

// truncateUTF8 cuts s to at most max bytes without splitting a character.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
