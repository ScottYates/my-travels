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
