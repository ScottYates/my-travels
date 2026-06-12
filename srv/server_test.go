package srv

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv.exe.dev/db/dbgen"
)

// baseDir is the project root. New() looks for templates under baseDir/srv/templates
// and static files under baseDir/srv/static. The tests must run with the package
// working directory set to baseDir, which is one level up from the srv/ package.
const baseDir = ".."

// newTestServer returns a server backed by a fresh on-disk SQLite database in a
// temp directory, with the upload directory also redirected to a temp dir, and the
// server's HTTP routes wired up exactly as Serve wires them in production (csrfCheck
// included). The caller is responsible for the lifetime of the returned server.
func newTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.sqlite3")

	// Sandbox the upload directory so tests don't write into the repo.
	uploadDir := filepath.Join(tempDir, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatalf("create upload dir: %v", err)
	}

	s, err := New(dbPath, "test-hostname", "test-google-client-id", "test-google-client-secret", "admin@test.com", baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Force the upload dir into the sandbox (New() joins it with baseDir).
	s.UploadDir = uploadDir
	// Close the DB when the test ends so the temp dir can be removed
	// (the sqlite handle holds a lock on the file on Windows).
	t.Cleanup(func() { s.DB.Close() })

	// Build the same handler chain Serve() builds in production.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /share/{shareID}", s.handleIndex)
	mux.HandleFunc("GET /present/{shareID}", s.handleIndex)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /auth/google/login", s.handleGoogleLogin)
	mux.HandleFunc("GET /auth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/trips", s.handleListTrips)
	mux.HandleFunc("POST /api/trips", s.handleCreateTrip)
	mux.HandleFunc("GET /api/trips/{id}", s.handleGetTrip)
	mux.HandleFunc("PUT /api/trips/{id}", s.handleUpdateTrip)
	mux.HandleFunc("DELETE /api/trips/{id}", s.handleDeleteTrip)
	mux.HandleFunc("GET /api/share/{shareID}", s.handleGetTripByShareID)
	mux.HandleFunc("PUT /api/trips/{id}/present-slug", s.handleUpdatePresentSlug)
	mux.HandleFunc("GET /api/present/{slug}", s.handleGetTripByPresentSlug)
	mux.HandleFunc("POST /api/trips/{id}/stops", s.handleCreateStop)
	mux.HandleFunc("PUT /api/stops/{id}", s.handleUpdateStop)
	mux.HandleFunc("DELETE /api/stops/{id}", s.handleDeleteStop)
	mux.HandleFunc("POST /api/photos/{photoID}/comments", s.handleCreateComment)
	mux.HandleFunc("PUT /api/comments/{id}", s.handleUpdateComment)
	mux.HandleFunc("DELETE /api/comments/{id}", s.handleDeleteComment)
	mux.HandleFunc("POST /api/admin/impersonate", s.handleAdminImpersonate)
	mux.HandleFunc("POST /api/admin/stop-impersonate", s.handleAdminStopImpersonate)

	var handler http.Handler = mux
	handler = s.csrfCheck(handler)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return s, ts, tempDir
}

// insertSession creates a session row directly in the DB and returns the token.
// Tests use this to simulate a logged-in user without going through OAuth.
func insertSession(t *testing.T, s *Server, userID, email string) string {
	t.Helper()
	q := dbgen.New(s.DB)
	token := "test-token-" + userID
	now := time.Now()
	if err := q.CreateSession(context.Background(), dbgen.CreateSessionParams{
		Token:     token,
		UserID:    userID,
		Email:     email,
		CreatedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: now.Add(time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return token
}

// cookieValue builds a *http.Cookie with the same attributes the server uses
// (Path=/, HttpOnly, SameSite=Lax) so the test request looks like a real one
// to the SameSite-aware browser and the csrfCheck middleware.
func cookieValue(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// doRequest is a small helper that signs requests with a session cookie and
// optional Origin header, and decodes the JSON body if dst is non-nil.
func doRequest(t *testing.T, ts *httptest.Server, method, path string, sessionToken, origin string, body any, dst any) *http.Response {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+path, reqBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sessionToken != "" {
		req.AddCookie(cookieValue("session", sessionToken))
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if dst != nil && resp.StatusCode < 400 && resp.ContentLength != 0 {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	} else {
		resp.Body.Close()
	}
	return resp
}

// ---------------------------------------------------------------------------
// Server initialization
// ---------------------------------------------------------------------------

// TestNew exercises the new (post-refactor) constructor signature: it must accept
// 6 arguments, initialize the DB, and create the upload directory.
func TestNew(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "init.sqlite3")

	s, err := New(dbPath, "test-hostname", "", "", "admin@test.com", baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	if s == nil {
		t.Fatal("New returned nil server")
	}
	if s.Hostname != "test-hostname" {
		t.Errorf("Hostname = %q, want %q", s.Hostname, "test-hostname")
	}
	if s.AdminEmail != "admin@test.com" {
		t.Errorf("AdminEmail = %q, want %q", s.AdminEmail, "admin@test.com")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db file not created: %v", err)
	}
}

// TestNewRequiresTemplates ensures New fails fast if it can't find the
// templates directory, so the user gets a clear error message in production
// if -base-dir is misconfigured.
func TestNewRequiresTemplates(t *testing.T) {
	tempDir := t.TempDir()
	_, err := New(filepath.Join(tempDir, "x.sqlite3"), "h", "", "", "a@b.com", tempDir)
	if err == nil {
		t.Fatal("expected error for missing templates dir, got nil")
	}
	if !strings.Contains(err.Error(), "templates") {
		t.Errorf("error should mention templates dir, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Static and SPA routes
// ---------------------------------------------------------------------------

func TestGetIndex(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", got)
	}
	body := readAll(t, resp)
	// The SPA injects AUTH_JSON and GOOGLE_CLIENT_ID. Verify both landed.
	if !strings.Contains(body, "test-google-client-id") {
		t.Error("index.html should include the configured GoogleClientID")
	}
	if !strings.Contains(body, "__authInfo") {
		t.Error("index.html should include __authInfo (the AUTH_JSON injection point)")
	}
	// Unauthenticated means authenticated:false and no impersonation banner.
	if !strings.Contains(body, `"authenticated":false`) {
		t.Error("unauthenticated page should set authenticated:false in AUTH_JSON")
	}
	if strings.Contains(body, "Stop impersonating") {
		t.Error("unauthenticated page should not show impersonation banner")
	}
}

func TestGetShareRouteRendersIndex(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/share/abc123")
	if err != nil {
		t.Fatalf("GET /share/abc123: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /share/abc123 status = %d, want 200", resp.StatusCode)
	}
}

func TestGetPresentRouteRendersIndex(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/present/abc123")
	if err != nil {
		t.Fatalf("GET /present/abc123: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /present/abc123 status = %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// /api/me and authentication state
// ---------------------------------------------------------------------------

// TestMeUnauthenticated verifies that /api/me reports authenticated:false for a
// request with no session cookie, which is what the SPA uses to show the login
// button.
func TestMeUnauthenticated(t *testing.T) {
	_, ts, _ := newTestServer(t)
	var got map[string]any
	resp := doRequest(t, ts, "GET", "/api/me", "", "", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if auth, _ := got["authenticated"].(bool); auth {
		t.Errorf("authenticated = true, want false; body: %v", got)
	}
}

// TestMeAsRegularUser verifies /api/me for a non-admin user.
func TestMeAsRegularUser(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	var got map[string]any
	resp := doRequest(t, ts, "GET", "/api/me", tok, "", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", got["authenticated"])
	}
	if got["email"] != "alice@example.com" {
		t.Errorf("email = %v, want alice@example.com", got["email"])
	}
	if got["is_admin"] != false {
		t.Errorf("is_admin = %v, want false for non-admin", got["is_admin"])
	}
	// handleMe always emits the "impersonating" key (false here, true while
	// impersonating). The SPA reads it explicitly so it can show the banner.
	if imp, _ := got["impersonating"].(bool); imp {
		t.Errorf("impersonating = true for non-impersonating user; want false")
	}
}

// TestMeAsAdmin verifies /api/me sets is_admin=true when the email matches
// the configured admin email.
func TestMeAsAdmin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "admin-1", "admin@test.com")

	var got map[string]any
	resp := doRequest(t, ts, "GET", "/api/me", tok, "", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got["is_admin"] != true {
		t.Errorf("is_admin = %v, want true", got["is_admin"])
	}
	if got["impersonating"] != false {
		t.Errorf("impersonating = %v, want false when not impersonating", got["impersonating"])
	}
}

// ---------------------------------------------------------------------------
// Trips CRUD
// ---------------------------------------------------------------------------

// TestCreateAndListTrip is the happy path: a logged-in user creates a trip and
// then sees it in their list. Verifies ownership-based listing.
func TestCreateAndListTrip(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	var created map[string]any
	resp := doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{
		"title":       "Summer in Italy",
		"description": "Three weeks around the Amalfi coast",
	}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/trips status = %d, want 201", resp.StatusCode)
	}
	if created["title"] != "Summer in Italy" {
		t.Errorf("title = %v, want Summer in Italy", created["title"])
	}
	if created["user_id"] != "user-1" {
		t.Errorf("user_id = %v, want user-1", created["user_id"])
	}

	// Now list
	var listed []map[string]any
	resp = doRequest(t, ts, "GET", "/api/trips", tok, "", nil, &listed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/trips status = %d", resp.StatusCode)
	}
	if len(listed) != 1 {
		t.Errorf("len(trips) = %d, want 1", len(listed))
	}
}

// TestCreateTripRequiresAuth ensures anonymous users can't create trips.
func TestCreateTripRequiresAuth(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp := doRequest(t, ts, "POST", "/api/trips", "", ts.URL, map[string]any{"title": "x"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestCreateTripRequiresTitle ensures empty titles are rejected with 400.
func TestCreateTripRequiresTitle(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")
	resp := doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{"title": ""}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestGetTripForbiddenForOtherUser ensures the ownership check on
// /api/trips/{id} actually returns 403 when someone else asks for it.
func TestGetTripForbiddenForOtherUser(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tokA := insertSession(t, s, "user-a", "alice@example.com")
	tokB := insertSession(t, s, "user-b", "bob@example.com")

	var created map[string]any
	doRequest(t, ts, "POST", "/api/trips", tokA, ts.URL, map[string]any{"title": "A's trip"}, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, ts, "GET", "/api/trips/"+id, tokB, "", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for cross-user GET", resp.StatusCode)
	}
}

// TestDeleteTripAndVerify404 exercises the delete path and then confirms the
// trip is no longer reachable.
func TestDeleteTripAndVerify404(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	var created map[string]any
	doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{"title": "to delete"}, &created)
	id, _ := created["id"].(string)

	resp := doRequest(t, ts, "DELETE", "/api/trips/"+id, tok, ts.URL, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", resp.StatusCode)
	}

	resp = doRequest(t, ts, "GET", "/api/trips/"+id, tok, "", nil, nil)
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 403 or 404", resp.StatusCode)
	}
}

// TestShareRoutePublic verifies that /api/share/{id} returns trip data even
// when there is no session cookie (it's used by the public share page).
func TestShareRoutePublic(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	var created map[string]any
	doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{"title": "public"}, &created)
	shareID, _ := created["share_id"].(string)

	// No session token here - this is the public-facing path.
	var got map[string]any
	resp := doRequest(t, ts, "GET", "/api/share/"+shareID, "", "", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got["title"] != "public" {
		t.Errorf("title = %v, want public", got["title"])
	}
}

// TestShareRoute404 covers the "no such share" path.
func TestShareRoute404(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp := doRequest(t, ts, "GET", "/api/share/does-not-exist", "", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Stops CRUD
// ---------------------------------------------------------------------------

// TestCreateStop is a basic happy path for stop creation.
func TestCreateStop(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	var created map[string]any
	doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{"title": "trip"}, &created)
	tripID, _ := created["id"].(string)

	var stop map[string]any
	resp := doRequest(t, ts, "POST", "/api/trips/"+tripID+"/stops", tok, ts.URL, map[string]any{
		"title":      "Naples",
		"lat":        40.85,
		"lng":        14.27,
		"elevation":  0.0,
		"stop_order": 0,
	}, &stop)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if stop["title"] != "Naples" {
		t.Errorf("title = %v, want Naples", stop["title"])
	}
	if stop["trip_id"] != tripID {
		t.Errorf("trip_id = %v, want %s", stop["trip_id"], tripID)
	}
}

// TestUpdateStop verifies that a stop can be modified by its owner.
func TestUpdateStop(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	var trip map[string]any
	doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{"title": "t"}, &trip)
	tripID, _ := trip["id"].(string)

	var stop map[string]any
	doRequest(t, ts, "POST", "/api/trips/"+tripID+"/stops", tok, ts.URL, map[string]any{
		"title": "old", "lat": 1, "lng": 2, "stop_order": 0,
	}, &stop)
	stopID, _ := stop["id"].(string)

	resp := doRequest(t, ts, "PUT", "/api/stops/"+stopID, tok, ts.URL, map[string]any{
		"title": "new", "lat": 1.1, "lng": 2.2, "stop_order": 0,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PUT status = %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Impersonation (the original bug)
// ---------------------------------------------------------------------------

// TestImpersonationFlow exercises the full impersonation cycle. This is the
// flow whose UI button was broken by the IIFE-scoping bug.
//
//	1. Admin logs in.
//	2. Admin POSTs to /api/admin/impersonate with a target user.
//	3. Subsequent /api/me calls report the *target* user, with impersonating=true.
//	4. Admin POSTs to /api/admin/stop-impersonate.
//	5. /api/me reports the admin again, with impersonating=false.
//
// The test uses a real net/http CookieJar, the same way a real browser would,
// so the MaxAge=-1 cookie returned by stop-impersonate is automatically
// dropped from subsequent requests.
func TestImpersonationFlow(t *testing.T) {
	s, ts, _ := newTestServer(t)
	adminTok := insertSession(t, s, "admin-1", "admin@test.com")
	_ = insertSession(t, s, "user-2", "bob@example.com")

	// http.Client with a cookie jar so the server's Set-Cookie (including
	// MaxAge=-1 deletions) is honored the way a browser would honor it.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Seed the jar with the admin's session cookie.
	jarCookies := []*http.Cookie{cookieValue("session", adminTok)}
	u, _ := url.Parse(ts.URL)
	jar.SetCookies(u, jarCookies)

	// Step 1: admin is logged in normally.
	var me map[string]any
	getJSON(t, client, ts.URL+"/api/me", &me)
	if me["is_admin"] != true {
		t.Fatalf("precondition: admin should be admin; got %v", me)
	}
	if me["impersonating"] != false {
		t.Fatalf("precondition: not impersonating yet; got %v", me)
	}

	// Step 2: start impersonating bob.
	resp := postJSON(t, client, ts.URL+"/api/admin/impersonate", map[string]any{
		"user_id": "user-2",
		"email":   "bob@example.com",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/impersonate status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// The jar should now hold the impersonate cookie.
	jarImp := findCookie(jar.Cookies(u), "impersonate")
	if jarImp == nil {
		t.Fatal("cookie jar missing impersonate cookie after start")
	}
	if jarImp.Value != "user-2:bob@example.com" {
		t.Errorf("jar impersonate value = %q, want user-2:bob@example.com", jarImp.Value)
	}

	// Step 3: /api/me should now report bob.
	var asBob map[string]any
	getJSON(t, client, ts.URL+"/api/me", &asBob)
	if asBob["email"] != "bob@example.com" {
		t.Errorf("while impersonating, /api/me email = %v, want bob@example.com", asBob["email"])
	}
	if asBob["impersonating"] != true {
		t.Errorf("while impersonating, impersonating = %v, want true", asBob["impersonating"])
	}
	if asBob["is_admin"] != true {
		t.Errorf("while impersonating, is_admin should still be true (real user is admin); got %v", asBob["is_admin"])
	}
	if asBob["real_email"] != "admin@test.com" {
		t.Errorf("real_email = %v, want admin@test.com", asBob["real_email"])
	}

	// Step 4: stop impersonating. The browser's JS (after the bug fix) hits
	// /api/admin/stop-impersonate. The server must clear the cookie.
	resp2 := postJSON(t, client, ts.URL+"/api/admin/stop-impersonate", nil, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("stop-impersonate status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()

	// The cookie jar should have dropped the impersonate cookie (the server
	// sent back Set-Cookie: impersonate=; Max-Age=0).
	jarImp = findCookie(jar.Cookies(u), "impersonate")
	if jarImp != nil {
		t.Errorf("cookie jar should have dropped impersonate after stop; got %+v", jarImp)
	}

	// Step 5: a follow-up /api/me should report the admin again, not bob.
	var after map[string]any
	getJSON(t, client, ts.URL+"/api/me", &after)
	if after["email"] != "admin@test.com" {
		t.Errorf("after stop, /api/me email = %v, want admin@test.com", after["email"])
	}
	if after["impersonating"] != false {
		t.Errorf("after stop, impersonating = %v, want false", after["impersonating"])
	}
}

// TestImpersonateRequiresAdmin ensures a non-admin can't impersonate anyone.
// This is what stopped a stolen session cookie from being used to escalate
// to admin in the impersonation system.
func TestImpersonateRequiresAdmin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")
	resp := doRequest(t, ts, "POST", "/api/admin/impersonate", tok, ts.URL, map[string]any{
		"user_id": "user-2", "email": "bob@example.com",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestStopImpersonateRequiresAdmin ensures the same protection for the
// stop-impersonate endpoint.
func TestStopImpersonateRequiresAdmin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")
	resp := doRequest(t, ts, "POST", "/api/admin/stop-impersonate", tok, ts.URL, nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestImpersonateRequiresTarget ensures the body is validated.
func TestImpersonateRequiresTarget(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "admin-1", "admin@test.com")

	// Empty body.
	resp := doRequest(t, ts, "POST", "/api/admin/impersonate", tok, ts.URL, map[string]any{}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body status = %d, want 400", resp.StatusCode)
	}
}

// TestStopImpersonateClearsImpBannerInIndex is the regression test for the
// original bug. The impersonation banner in index.html is only meaningful
// when the request is from the real admin user with an active impersonation
// cookie. After /api/admin/stop-impersonate, the server should no longer
// report impersonating:true. (The full UI button click is exercised by the
// Playwright suite; this Go test exercises the same server logic at the API
// level.)
//
// Uses a real cookie jar so the MaxAge=-1 cookie the server returns is
// actually dropped, the way a real browser would.
func TestStopImpersonateClearsImpBannerInIndex(t *testing.T) {
	s, ts, _ := newTestServer(t)
	adminTok := insertSession(t, s, "admin-1", "admin@test.com")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	u, _ := url.Parse(ts.URL)
	jar.SetCookies(u, []*http.Cookie{
		cookieValue("session", adminTok),
		cookieValue("impersonate", "user-2:bob@example.com"),
	})

	// (1)+(2) before the click.
	before := loadIndexWithClient(t, client, ts.URL)
	if !strings.Contains(before, `"impersonating":true`) {
		t.Errorf("before stop-impersonate, AUTH_JSON should say impersonating:true; got: %s", extractAuth(before))
	}

	// (3) click - hit the API.
	resp := postJSON(t, client, ts.URL+"/api/admin/stop-impersonate", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if findCookie(jar.Cookies(u), "impersonate") != nil {
		t.Errorf("cookie jar should have dropped impersonate after stop")
	}

	// (4) Reload - the cookie is gone, so the page should not show the banner.
	after := loadIndexWithClient(t, client, ts.URL)
	if strings.Contains(after, `"impersonating":true`) {
		t.Errorf("after stop-impersonate, AUTH_JSON must not say impersonating:true; got: %s", extractAuth(after))
	}
	if !strings.Contains(after, `"is_admin":true`) {
		t.Errorf("after stop-impersonate, AUTH_JSON should still say is_admin:true; got: %s", extractAuth(after))
	}
}

// ---------------------------------------------------------------------------
// CSRF middleware
// ---------------------------------------------------------------------------

// TestCSRFBlocksCrossOrigin is the meat of the CSRF defense: a state-changing
// request (POST) from a different origin must be rejected with 403 even if
// it carries a valid session cookie.
func TestCSRFBlocksCrossOrigin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	// Cross-origin POST - should be blocked.
	req, _ := http.NewRequest("POST", ts.URL+"/api/trips", bytes.NewReader([]byte(`{"title":"hax"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	req.AddCookie(cookieValue("session", tok))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST status = %d, want 403", r.StatusCode)
	}
}

// TestCSRFAllowsSameOrigin ensures that POSTs from the same origin still go
// through. (If this fails, every browser POST to the API would break.)
func TestCSRFAllowsSameOrigin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")
	resp := doRequest(t, ts, "POST", "/api/trips", tok, ts.URL, map[string]any{"title": "x"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("same-origin POST status = %d, want 201", resp.StatusCode)
	}
}

// TestCSRFAllowsNoOrigin verifies the documented behavior: if the client
// doesn't send an Origin header at all (e.g. curl, server-to-server), the
// middleware relies on the SameSite=Lax cookie as primary defense and lets
// the request through. This is the same behavior as same-origin/cross-origin
// would observe in practice.
func TestCSRFAllowsNoOrigin(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")
	resp := doRequest(t, ts, "POST", "/api/trips", tok, "", map[string]any{"title": "x"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("no-origin POST status = %d, want 201", resp.StatusCode)
	}
}

// TestCSRFAllowsGet ensures GET requests bypass the CSRF check entirely.
func TestCSRFAllowsGet(t *testing.T) {
	_, ts, _ := newTestServer(t)
	// Even an absurd origin should be fine for GET.
	req, _ := http.NewRequest("GET", ts.URL+"/api/trips", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("GET status = %d, want 200", r.StatusCode)
	}
}

// TestCSRFBlocksBadReferer verifies the Referer-based fallback path: if Origin
// is missing but Referer is present, it must start with the public origin.
func TestCSRFBlocksBadReferer(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	req, _ := http.NewRequest("POST", ts.URL+"/api/trips", bytes.NewReader([]byte(`{"title":"hax"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://evil.example.com/some/page")
	req.AddCookie(cookieValue("session", tok))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("bad-referer POST status = %d, want 403", r.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

// TestLogoutClearsSession verifies the logout endpoint deletes the session
// row and clears the cookie. After logout, /api/me should report
// unauthenticated.
func TestLogoutClearsSession(t *testing.T) {
	s, ts, _ := newTestServer(t)
	tok := insertSession(t, s, "user-1", "alice@example.com")

	resp := doRequest(t, ts, "POST", "/auth/logout", tok, ts.URL, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /auth/logout status = %d, want 200", resp.StatusCode)
	}
	cleared := findCookie(resp.Cookies(), "session")
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("session cookie should be cleared; got %+v", cleared)
	}

	// Confirm the session row is gone (use a raw HTTP call with the now-cleared
	// cookie value - the server should still see it as an unknown token).
	var me map[string]any
	r2 := doRequest(t, ts, "GET", "/api/me", tok, "", nil, &me)
	r2.Body.Close()
	if me["authenticated"] == true {
		t.Errorf("after logout, /api/me authenticated = %v, want false", me["authenticated"])
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readAll reads the body of resp.Body into a string. Exits the test on error.
func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	for {
		n, err := r.Body.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

// findCookie returns the cookie with the given name from cs, or nil.
func findCookie(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// extractAuth pulls the substring between the const AUTH_JSON = and the
// closing semicolon in the rendered index.html, so test failures can show
// the actual JSON the browser will see.
func extractAuth(html string) string {
	const marker = "const AUTH_JSON ="
	i := strings.Index(html, marker)
	if i < 0 {
		return "(AUTH_JSON not found)"
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, ";")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// loadIndexWithClient fetches / and returns the body using the given
// http.Client (which carries a cookie jar if you want the test to behave
// like a real browser with respect to Set-Cookie).
func loadIndexWithClient(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	r, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	return readAll(t, r)
}

// getJSON does a GET, asserts 200, and decodes the body into dst.
func getJSON(t *testing.T, client *http.Client, url string, dst any) {
	t.Helper()
	r, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, r.StatusCode)
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// postJSON does a POST with a JSON body (or empty if body==nil) and decodes
// the response into dst if dst is non-nil.
func postJSON(t *testing.T, client *http.Client, url string, body any, dst any) *http.Response {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequest("POST", url, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	if dst != nil && resp.StatusCode < 400 {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			resp.Body.Close()
			t.Fatalf("decode: %v", err)
		}
	}
	return resp
}

// (no helper assertions needed; tests are direct)
