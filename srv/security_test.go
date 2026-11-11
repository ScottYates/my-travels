package srv

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"srv.exe.dev/db/dbgen"
)

// testJPEG returns a small valid JPEG.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// postMultipart sends one file in a multipart form and returns the response.
func postMultipart(t *testing.T, ts *httptest.Server, path, token, field, fileName string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, fileName)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	fw.Write(data)
	mw.Close()

	req, err := http.NewRequest("POST", ts.URL+path, &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.AddCookie(cookieValue("session", token))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func createTestTrip(t *testing.T, ts *httptest.Server, token string) dbgen.Trip {
	t.Helper()
	var trip dbgen.Trip
	resp := doRequest(t, ts, "POST", "/api/trips", token, "", map[string]string{"title": "T"}, &trip)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create trip: status %d", resp.StatusCode)
	}
	return trip
}

func uploadTestPhoto(t *testing.T, ts *httptest.Server, token, tripID, name string) dbgen.Photo {
	t.Helper()
	resp := postMultipart(t, ts, "/api/trips/"+tripID+"/photos", token, "photo", name, testJPEG(t))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload %s: status %d: %s", name, resp.StatusCode, b)
	}
	var p dbgen.Photo
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode photo: %v", err)
	}
	return p
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// The upload directory must not be listable, and must not serve chunk
// temp files or anything outside a single file name.
func TestUploadsNoListingOrTraversal(t *testing.T) {
	s, ts, _ := newTestServer(t)
	os.MkdirAll(filepath.Join(s.UploadDir, "thumbs"), 0o755)
	os.WriteFile(filepath.Join(s.UploadDir, "a.jpg"), testJPEG(t), 0o644)
	os.WriteFile(filepath.Join(s.UploadDir, "_chunks_abc"), []byte("x"), 0o644)

	for _, p := range []string{
		"/uploads/", "/uploads/thumbs/", "/uploads/thumbs", "/uploads/_chunks_abc",
		"/uploads/..%2fsecret", "/uploads/thumbs/..%2fa.jpg", "/uploads/x/a.jpg",
	} {
		resp := get(t, ts.URL+p)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
		if strings.Contains(string(body), "a.jpg") {
			t.Errorf("GET %s: response lists directory contents", p)
		}
	}
}

// Files already on disk with a non-image extension must be sent as a
// download, never rendered.
func TestUploadsUnknownTypeServedAsAttachment(t *testing.T) {
	s, ts, _ := newTestServer(t)
	os.WriteFile(filepath.Join(s.UploadDir, "old.html"), []byte("<script>alert(1)</script>"), 0o644)
	// HTML content behind an allowed extension must keep the image type.
	os.WriteFile(filepath.Join(s.UploadDir, "fake.heic"), []byte("<html><script>alert(1)</script>"), 0o644)

	resp := get(t, ts.URL+"/uploads/old.html")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("old.html Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("old.html Content-Disposition = %q", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("old.html missing nosniff")
	}

	resp = get(t, ts.URL+"/uploads/fake.heic")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "image/heic" {
		t.Errorf("fake.heic Content-Type = %q, want image/heic", ct)
	}
}

func TestUploadRejectsDisallowedExtension(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")
	trip := createTestTrip(t, ts, token)

	for _, name := range []string{"evil.html", "evil.svg", "evil.js", `x.jpg" onerror="alert(1)`} {
		resp := postMultipart(t, ts, "/api/trips/"+trip.ID+"/photos", token, "photo", name, []byte("<script>alert(1)</script>"))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("upload %q: status %d, want 415", name, resp.StatusCode)
		}
	}

	resp := doRequest(t, ts, "POST", "/api/trips/"+trip.ID+"/uploads/init", token, "",
		map[string]any{"file_name": "evil.svg", "mime_type": "image/svg+xml", "total_size": 10}, nil)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("chunked init evil.svg: status %d, want 415", resp.StatusCode)
	}

	entries, _ := os.ReadDir(s.UploadDir)
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("unexpected file left in upload dir: %s", e.Name())
		}
	}
}

// A shared trip must stay viewable with no login: trip data, full-size
// file and thumbnail.
func TestSharedTripPhotoViewableWithoutLogin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")
	trip := createTestTrip(t, ts, token)
	photo := uploadTestPhoto(t, ts, token, trip.ID, "IMG_1.JPG")

	resp := get(t, ts.URL+"/api/share/"+trip.ShareID)
	var detail tripDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode share: %v", err)
	}
	resp.Body.Close()
	if len(detail.Photos) != 1 || detail.Photos[0].ID != photo.ID {
		t.Fatalf("share response photos = %+v", detail.Photos)
	}

	resp = get(t, ts.URL+"/uploads/"+photo.Filename)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("GET upload: status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	resp = get(t, ts.URL+"/thumb/"+photo.Filename)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET thumb: status %d", resp.StatusCode)
	}
	// The cached thumbnail is also reachable under /uploads/thumbs/.
	resp = get(t, ts.URL+"/uploads/thumbs/"+photo.Filename)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /uploads/thumbs: status %d", resp.StatusCode)
	}
}

func TestRouteColorSanitised(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")
	trip := createTestTrip(t, ts, token)

	var route dbgen.Route
	doRequest(t, ts, "POST", "/api/trips/"+trip.ID+"/routes", token, "",
		map[string]string{"name": "r", "geojson": "{}", "color": `red" onmouseover="alert(1)`}, &route)
	if route.Color != defaultRouteColor {
		t.Errorf("color = %q, want default", route.Color)
	}
	doRequest(t, ts, "POST", "/api/trips/"+trip.ID+"/routes", token, "",
		map[string]string{"name": "r", "geojson": "{}", "color": "#ff0000"}, &route)
	if route.Color != "#ff0000" {
		t.Errorf("color = %q, want #ff0000", route.Color)
	}
}

// buildImportZip returns a backup archive with the given manifest and files.
func buildImportZip(t *testing.T, manifest backupManifest, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mf, _ := zw.Create("manifest.json")
	json.NewEncoder(mf).Encode(manifest)
	for name, data := range files {
		f, _ := zw.Create(name)
		f.Write(data)
	}
	zw.Close()
	return buf.Bytes()
}

// An imported manifest is untrusted: a file with a non-image extension
// must not end up servable as HTML.
func TestImportNeutralisesDisallowedExtension(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")

	manifest := backupManifest{
		Version: 1,
		Trip:    dbgen.Trip{Title: "imported"},
		Photos: []dbgen.Photo{
			{ID: "p1", Filename: "evil.html"},
			{ID: "p2", Filename: "ok.jpg"},
		},
		Routes: []dbgen.Route{{ID: "r1", Name: "r", Geojson: "{}", Color: `x" onload="alert(1)`}},
	}
	data := buildImportZip(t, manifest, map[string][]byte{
		"files/evil.html": []byte("<script>alert(1)</script>"),
		"files/ok.jpg":    testJPEG(t),
	})
	resp := postMultipart(t, ts, "/api/trips/import", token, "file", "trip.zip", data)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("import: status %d: %s", resp.StatusCode, b)
	}
	var detail tripDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Photos) != 2 {
		t.Fatalf("photos = %d, want 2", len(detail.Photos))
	}
	for _, p := range detail.Photos {
		ext := filepath.Ext(p.Filename)
		if ext != ".jpg" && ext != unknownUploadExt {
			t.Errorf("imported filename %q kept a disallowed extension", p.Filename)
		}
		r := get(t, ts.URL+"/uploads/"+p.Filename)
		r.Body.Close()
		if strings.HasPrefix(r.Header.Get("Content-Type"), "text/") {
			t.Errorf("%s served as %s", p.Filename, r.Header.Get("Content-Type"))
		}
	}
	if len(detail.Routes) != 1 || detail.Routes[0].Color != defaultRouteColor {
		t.Errorf("imported route colour not sanitised: %+v", detail.Routes)
	}
}

// ---------------------------------------------------------------------------
// Ownership checks
// ---------------------------------------------------------------------------

// twoUsers returns a server with two logged-in users and one trip each.
func twoUsers(t *testing.T) (s *Server, ts *httptest.Server, tokA, tokB string, tripA, tripB dbgen.Trip) {
	t.Helper()
	s, ts, _ = newTestServer(t)
	tokA = insertSession(t, s, "userA", "a@test.com")
	tokB = insertSession(t, s, "userB", "b@test.com")
	tripA = createTestTrip(t, ts, tokA)
	tripB = createTestTrip(t, ts, tokB)
	return
}

func TestDeleteRouteRequiresOwner(t *testing.T) {
	_, ts, tokA, tokB, tripA, _ := twoUsers(t)

	var route dbgen.Route
	doRequest(t, ts, "POST", "/api/trips/"+tripA.ID+"/routes", tokA, "",
		map[string]string{"name": "r", "geojson": "{}"}, &route)

	if resp := doRequest(t, ts, "DELETE", "/api/routes/"+route.ID, tokB, "", nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("other user delete: status %d, want 403", resp.StatusCode)
	}
	if resp := doRequest(t, ts, "DELETE", "/api/routes/nope", tokA, "", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing route: status %d, want 404", resp.StatusCode)
	}
	if resp := doRequest(t, ts, "DELETE", "/api/routes/"+route.ID, tokA, "", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("owner delete: status %d, want 204", resp.StatusCode)
	}
}

func TestReorderRejectsForeignPhotos(t *testing.T) {
	s, ts, tokA, tokB, tripA, tripB := twoUsers(t)
	victim := uploadTestPhoto(t, ts, tokA, tripA.ID, "a.jpg")
	own := uploadTestPhoto(t, ts, tokB, tripB.ID, "b.jpg")

	resp := doRequest(t, ts, "POST", "/api/photos/reorder", tokB, "",
		map[string]any{"photo_ids": []string{own.ID, victim.ID}}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("reorder with foreign photo: status %d, want 400", resp.StatusCode)
	}

	// A new stop must not be able to take another trip's photo either.
	resp = doRequest(t, ts, "POST", "/api/trips/"+tripB.ID+"/stops", tokB, "",
		map[string]any{"title": "s", "lat": 1.0, "lng": 1.0, "photo_ids": []string{victim.ID}}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create stop with foreign photo: status %d, want 400", resp.StatusCode)
	}

	got, err := dbgen.New(s.DB).GetPhoto(t.Context(), victim.ID)
	if err != nil {
		t.Fatalf("GetPhoto: %v", err)
	}
	if got.StopID != nil || got.PhotoOrder != 0 {
		t.Errorf("victim photo was modified: stop=%v order=%d", got.StopID, got.PhotoOrder)
	}

	// Reordering within one's own trip still works.
	own2 := uploadTestPhoto(t, ts, tokB, tripB.ID, "c.jpg")
	resp = doRequest(t, ts, "POST", "/api/photos/reorder", tokB, "",
		map[string]any{"photo_ids": []string{own2.ID, own.ID}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("own reorder: status %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Size limits
// ---------------------------------------------------------------------------

func postRaw(t *testing.T, ts *httptest.Server, path, token string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.AddCookie(cookieValue("session", token))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func chunkedInit(t *testing.T, ts *httptest.Server, token, tripID, name string, total int64) (string, int) {
	t.Helper()
	var out struct {
		UploadID string `json:"upload_id"`
	}
	resp := doRequest(t, ts, "POST", "/api/trips/"+tripID+"/uploads/init", token, "",
		map[string]any{"file_name": name, "mime_type": "image/jpeg", "total_size": total}, &out)
	return out.UploadID, resp.StatusCode
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")
	trip := createTestTrip(t, ts, token)
	data := testJPEG(t)

	id, status := chunkedInit(t, ts, token, trip.ID, "pic.jpg", int64(len(data)))
	if status != http.StatusCreated {
		t.Fatalf("init: status %d", status)
	}
	half := len(data) / 2
	for _, part := range [][]byte{data[:half], data[half:]} {
		resp := postRaw(t, ts, "/api/uploads/"+id+"/chunk", token, part)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chunk: status %d", resp.StatusCode)
		}
	}
	var photo dbgen.Photo
	resp := doRequest(t, ts, "POST", "/api/uploads/"+id+"/complete", token, "", nil, &photo)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("complete: status %d", resp.StatusCode)
	}
	if photo.SizeBytes != int64(len(data)) || photo.Width != 8 || photo.Height != 8 {
		t.Errorf("photo = size %d %dx%d", photo.SizeBytes, photo.Width, photo.Height)
	}
	stored, err := os.ReadFile(filepath.Join(s.UploadDir, photo.Filename))
	if err != nil || !bytes.Equal(stored, data) {
		t.Errorf("stored file differs from upload (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(s.UploadDir, "_chunks_"+id)); !os.IsNotExist(err) {
		t.Errorf("temp chunk file left behind")
	}
}

func TestChunkedUploadEnforcesDeclaredSize(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")
	trip := createTestTrip(t, ts, token)

	if _, status := chunkedInit(t, ts, token, trip.ID, "big.mp4", maxChunkedUploadBytes+1); status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized init: status %d, want 413", status)
	}

	// More bytes than declared: the upload is dropped.
	id, _ := chunkedInit(t, ts, token, trip.ID, "pic.jpg", 10)
	resp := postRaw(t, ts, "/api/uploads/"+id+"/chunk", token, make([]byte, 11))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("overflow chunk: status %d, want 413", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(s.UploadDir, "_chunks_"+id)); !os.IsNotExist(err) {
		t.Errorf("temp file kept after overflow")
	}

	// Fewer bytes than declared: complete is refused.
	id, _ = chunkedInit(t, ts, token, trip.ID, "pic.jpg", 10)
	resp = postRaw(t, ts, "/api/uploads/"+id+"/chunk", token, make([]byte, 5))
	resp.Body.Close()
	resp = doRequest(t, ts, "POST", "/api/uploads/"+id+"/complete", token, "", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short complete: status %d, want 400", resp.StatusCode)
	}

	// One chunk over the per-chunk cap.
	id, _ = chunkedInit(t, ts, token, trip.ID, "v.mp4", maxChunkBytes*2)
	resp = postRaw(t, ts, "/api/uploads/"+id+"/chunk", token, make([]byte, maxChunkBytes+1))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized chunk: status %d, want 413", resp.StatusCode)
	}
}

func TestJSONBodyLimit(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")
	trip := createTestTrip(t, ts, token)
	photo := uploadTestPhoto(t, ts, token, trip.ID, "a.jpg")

	big := strings.Repeat("a", maxCommentBodyBytes+1)
	resp := doRequest(t, ts, "POST", "/api/share/"+trip.ShareID+"/photos/"+photo.ID+"/comments", "", "",
		map[string]string{"author": "x", "body": big}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized comment: status %d, want 400", resp.StatusCode)
	}
	// A normal anonymous comment on a shared trip still works.
	resp = doRequest(t, ts, "POST", "/api/share/"+trip.ShareID+"/photos/"+photo.ID+"/comments", "", "",
		map[string]string{"author": "x", "body": "nice"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("normal comment: status %d, want 201", resp.StatusCode)
	}

	resp = doRequest(t, ts, "POST", "/api/trips", token, "",
		map[string]string{"title": strings.Repeat("a", maxJSONBodyBytes+1)}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized trip body: status %d, want 400", resp.StatusCode)
	}
}

// A zip entry that declares more data than the import limit is refused
// before anything is created.
func TestImportRejectsOversizedArchive(t *testing.T) {
	s, ts, _ := newTestServer(t)
	token := insertSession(t, s, "u1", "u1@test.com")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mf, _ := zw.Create("manifest.json")
	json.NewEncoder(mf).Encode(backupManifest{Version: 1, Trip: dbgen.Trip{Title: "bomb"}})
	// Raw entry: tiny payload, huge declared size.
	fw, _ := zw.CreateRaw(&zip.FileHeader{Name: "files/x.jpg", Method: zip.Deflate,
		CompressedSize64: 1, UncompressedSize64: maxImportBytes + 1})
	fw.Write([]byte{0})
	zw.Close()

	resp := postMultipart(t, ts, "/api/trips/import", token, "file", "trip.zip", buf.Bytes())
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("import: status %d, want 413", resp.StatusCode)
	}
	var trips []dbgen.Trip
	doRequest(t, ts, "GET", "/api/trips", token, "", nil, &trips)
	if len(trips) != 0 {
		t.Errorf("trip created from rejected archive")
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp := get(t, ts.URL+"/")
	resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing nosniff")
	}
	if resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("app page not frame-protected")
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "object-src 'none'") {
		t.Errorf("CSP = %q", resp.Header.Get("Content-Security-Policy"))
	}

	// Shared pages stay embeddable.
	resp = get(t, ts.URL+"/share/abc")
	resp.Body.Close()
	if resp.Header.Get("X-Frame-Options") != "" || strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors") {
		t.Errorf("share page should not carry frame restrictions")
	}
}

func TestNoAdminWithoutAdminEmail(t *testing.T) {
	s, ts, _ := newTestServer(t)
	s.AdminEmail = ""
	token := insertSession(t, s, "u1", "u1@test.com")
	resp := doRequest(t, ts, "POST", "/api/admin/impersonate", token, "",
		map[string]string{"user_id": "x", "email": "x@test.com"}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}
