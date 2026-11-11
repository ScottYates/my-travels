package srv

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// isAdmin returns true if the real (non-impersonated) user is the admin.
func (s *Server) isAdmin(r *http.Request) bool {
	u := s.getRealUser(r)
	return u != nil && s.isAdminEmail(u.Email)
}

// requireAdmin checks if the current user is admin; returns false and sends 403 if not.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.isAdmin(r) {
		jsonError(w, "admin access required", http.StatusForbidden)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Admin page (HTML)
// ---------------------------------------------------------------------------

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	tmpl, err := template.ParseFiles(filepath.Join(s.TemplatesDir, "admin.html"))
	if err != nil {
		slog.Error("admin: parse template", "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, nil)
}

// ---------------------------------------------------------------------------
// Admin API: Logs
// ---------------------------------------------------------------------------

type logEntry struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	// Detail is the remaining attributes as "key=value" pairs.
	Detail string `json:"detail"`
	// File is the log file the line came from (upload issues only).
	File string `json:"file,omitempty"`
	Raw  string `json:"raw"`
}

// maxUploadIssues caps how many log lines the upload issues panel returns.
const maxUploadIssues = 500

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	logDir := s.LogDir

	// List available log files
	var logFiles []string
	filepath.WalkDir(logDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".log") {
			logFiles = append(logFiles, d.Name())
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(logFiles)))

	// Which file to read
	date := r.URL.Query().Get("date")
	if date == "" && len(logFiles) > 0 {
		date = strings.TrimSuffix(logFiles[0], ".log")
	}

	// Filter
	levelFilter := r.URL.Query().Get("level") // INFO, WARN, ERROR, or empty for all
	searchFilter := r.URL.Query().Get("search")

	// Pagination
	limitStr := r.URL.Query().Get("limit")
	limit := 200
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 5000 {
		limit = l
	}

	var entries []logEntry

	if date != "" {
		// Strict date validation: only YYYY-MM-DD. The admin UI sends a
		// date picker value, but an attacker with admin access could
		// otherwise inject path separators into this query param. We
		// refuse anything that doesn't match the expected format rather
		// than rely on the logDir containment.
		if matched, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2}$`, date); !matched {
			jsonError(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		logPath := filepath.Join(logDir, date+".log") // #nosec G703 -- date is regex-validated above (line 109).
		data, err := os.ReadFile(logPath)             // #nosec G304,G703 -- date is regex-validated above; ReadFile is bounded by logDir.
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			// Read from the end (most recent first)
			for i := len(lines) - 1; i >= 0 && len(entries) < limit; i-- {
				line := lines[i]
				if line == "" {
					continue
				}

				entry := parseSlogLine(line)

				// Apply filters
				if levelFilter != "" && entry.Level != levelFilter {
					continue
				}
				if searchFilter != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(searchFilter)) {
					continue
				}

				entries = append(entries, entry)
			}
		}
	}

	jsonOK(w, map[string]any{
		"files":   logFiles,
		"date":    date,
		"entries": entries,
		"total":   len(entries),
	})
}

// ---------------------------------------------------------------------------
// Admin API: Users and their trips
// ---------------------------------------------------------------------------

type adminUser struct {
	UserID     string      `json:"user_id"`
	Email      string      `json:"email"`
	TripCount  int         `json:"trip_count"`
	PhotoCount int         `json:"photo_count"`
	StopCount  int         `json:"stop_count"`
	Trips      []adminTrip `json:"trips"`
}

type adminTrip struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	ShareID     string  `json:"share_id"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	StopCount   int     `json:"stop_count"`
	PhotoCount  int     `json:"photo_count"`
	VideoCount  int     `json:"video_count"`
	UserID      *string `json:"user_id"`
	UserEmail   string  `json:"user_email"`
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	// Every user we know of: anyone recorded at login, anyone with a
	// session, and anyone who owns a trip. Trip owners are included so a
	// trip is never hidden just because its owner's sessions have been
	// purged; for those the email may be unknown until they log in again.
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT ids.user_id,
		       COALESCE(u.email,
		                (SELECT s.email FROM sessions s WHERE s.user_id = ids.user_id ORDER BY s.created_at DESC LIMIT 1),
		                '') AS email
		FROM (
			SELECT user_id FROM users
			UNION SELECT user_id FROM sessions
			UNION SELECT user_id FROM trips WHERE user_id IS NOT NULL
		) ids
		LEFT JOIN users u ON u.user_id = ids.user_id
		ORDER BY email = '', email
	`)
	if err != nil {
		slog.Error("admin: list users", "error", err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var users []adminUser
	for rows.Next() {
		var u adminUser
		if err := rows.Scan(&u.UserID, &u.Email); err != nil {
			continue
		}
		if u.Email == "" {
			u.Email = "(email unknown) " + u.UserID
		}

		// Get trips for this user
		tripRows, err := s.DB.QueryContext(r.Context(), `
			SELECT t.id, t.title, t.description, t.share_id, t.created_at, t.updated_at, t.user_id,
			       (SELECT COUNT(*) FROM stops WHERE trip_id = t.id) as stop_count,
			       (SELECT COUNT(*) FROM photos WHERE trip_id = t.id AND is_video = 0) as photo_count,
			       (SELECT COUNT(*) FROM photos WHERE trip_id = t.id AND is_video = 1) as video_count
			FROM trips t WHERE t.user_id = ? ORDER BY t.updated_at DESC
		`, u.UserID)
		if err == nil {
			for tripRows.Next() {
				var t adminTrip
				if err := tripRows.Scan(&t.ID, &t.Title, &t.Description, &t.ShareID, &t.CreatedAt, &t.UpdatedAt, &t.UserID, &t.StopCount, &t.PhotoCount, &t.VideoCount); err != nil {
					continue
				}
				t.UserEmail = u.Email
				u.TripCount++
				u.PhotoCount += t.PhotoCount
				u.StopCount += t.StopCount
				u.Trips = append(u.Trips, t)
			}
			tripRows.Close()
		}

		users = append(users, u)
	}

	// Also get orphaned trips (no user_id)
	orphanRows, err := s.DB.QueryContext(r.Context(), `
		SELECT t.id, t.title, t.description, t.share_id, t.created_at, t.updated_at,
		       (SELECT COUNT(*) FROM stops WHERE trip_id = t.id) as stop_count,
		       (SELECT COUNT(*) FROM photos WHERE trip_id = t.id AND is_video = 0) as photo_count,
		       (SELECT COUNT(*) FROM photos WHERE trip_id = t.id AND is_video = 1) as video_count
		FROM trips t WHERE t.user_id IS NULL ORDER BY t.updated_at DESC
	`)
	var orphanTrips []adminTrip
	if err == nil {
		for orphanRows.Next() {
			var t adminTrip
			if err := orphanRows.Scan(&t.ID, &t.Title, &t.Description, &t.ShareID, &t.CreatedAt, &t.UpdatedAt, &t.StopCount, &t.PhotoCount, &t.VideoCount); err != nil {
				continue
			}
			t.UserEmail = "(no owner)"
			orphanTrips = append(orphanTrips, t)
		}
		orphanRows.Close()
	}

	jsonOK(w, map[string]any{
		"users":        users,
		"orphan_trips": orphanTrips,
	})
}

// ---------------------------------------------------------------------------
// Admin API: Upload / Disk stats
// ---------------------------------------------------------------------------

type uploadStats struct {
	TotalFiles     int      `json:"total_files"`
	TotalSizeBytes int64    `json:"total_size_bytes"`
	TotalSizeHuman string   `json:"total_size_human"`
	PhotoFiles     int      `json:"photo_files"`
	VideoFiles     int      `json:"video_files"`
	ThumbFiles     int      `json:"thumb_files"`
	OrphanFiles    []string `json:"orphan_files"`
	// OrphanSizeBytes is the combined size of OrphanFiles.
	OrphanSizeBytes int64    `json:"orphan_size_bytes"`
	OrphanSizeHuman string   `json:"orphan_size_human"`
	MissingFiles    []string `json:"missing_files"`
}

func (s *Server) handleAdminUploads(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	stats := uploadStats{}

	// Walk the uploads directory
	var diskFiles = make(map[string]bool)
	diskSizes := make(map[string]int64)
	filepath.WalkDir(s.UploadDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(s.UploadDir, path)

		// Skip thumbs directory for orphan check
		if strings.HasPrefix(rel, "thumbs") {
			stats.ThumbFiles++
			info, _ := d.Info()
			if info != nil {
				stats.TotalSizeBytes += info.Size()
			}
			stats.TotalFiles++
			return nil
		}

		info, _ := d.Info()
		if info != nil {
			stats.TotalSizeBytes += info.Size()
		}
		stats.TotalFiles++
		diskFiles[d.Name()] = true
		if info != nil {
			diskSizes[d.Name()] = info.Size()
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".mp4", ".mov", ".avi", ".webm", ".mkv":
			stats.VideoFiles++
		default:
			stats.PhotoFiles++
		}
		return nil
	})

	stats.TotalSizeHuman = humanBytes(stats.TotalSizeBytes)

	// Get all filenames from DB. If this fails, stop: an empty set would
	// report every file on disk as an orphan.
	dbFiles, err := s.photoFilenames(r.Context())
	if err != nil {
		slog.Error("admin: list photo filenames", "error", err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	// Orphan files: on disk but not in DB
	for fn := range diskFiles {
		if !dbFiles[fn] {
			stats.OrphanFiles = append(stats.OrphanFiles, fn)
			stats.OrphanSizeBytes += diskSizes[fn]
		}
	}
	sort.Strings(stats.OrphanFiles)
	stats.OrphanSizeHuman = humanBytes(stats.OrphanSizeBytes)

	// Missing files: in DB but not on disk
	for fn := range dbFiles {
		if !diskFiles[fn] {
			stats.MissingFiles = append(stats.MissingFiles, fn)
		}
	}
	sort.Strings(stats.MissingFiles)

	jsonOK(w, stats)
}

func humanBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ---------------------------------------------------------------------------
// Admin API: Database overview
// ---------------------------------------------------------------------------

type dbStats struct {
	Trips       int    `json:"trips"`
	Stops       int    `json:"stops"`
	Photos      int    `json:"photos"`
	Videos      int    `json:"videos"`
	Comments    int    `json:"comments"`
	Routes      int    `json:"routes"`
	Sessions    int    `json:"sessions"`
	Visitors    int    `json:"visitors"`
	DBSize      int64  `json:"db_size_bytes"`
	DBSizeHuman string `json:"db_size_human"`
}

func (s *Server) handleAdminDBStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	var stats dbStats
	countTable := func(table string) int {
		var n int
		s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n)
		return n
	}

	stats.Trips = countTable("trips")
	stats.Stops = countTable("stops")
	stats.Comments = countTable("comments")
	stats.Routes = countTable("routes")
	stats.Sessions = countTable("sessions")
	stats.Visitors = countTable("visitors")

	// Photos vs videos
	s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM photos WHERE is_video = 0").Scan(&stats.Photos)
	s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM photos WHERE is_video = 1").Scan(&stats.Videos)

	// DB file size
	var dbPath string
	s.DB.QueryRowContext(r.Context(), "PRAGMA database_list").Scan(new(int), new(string), &dbPath)
	if fi, err := os.Stat(dbPath); err == nil {
		stats.DBSize = fi.Size()
		stats.DBSizeHuman = humanBytes(fi.Size())
	}

	jsonOK(w, stats)
}

// ---------------------------------------------------------------------------
// Admin API: Recent upload errors from logs
// ---------------------------------------------------------------------------

func (s *Server) handleAdminUploadIssues(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	logDir := s.LogDir

	// Scan log files for upload-related errors and warnings
	var issues []logEntry

	var logFiles []string
	filepath.WalkDir(logDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".log") {
			logFiles = append(logFiles, d.Name())
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(logFiles)))

	// Search recent log files (max 5)
	max := 5
	if len(logFiles) < max {
		max = len(logFiles)
	}
	for _, lf := range logFiles[:max] {
		data, err := os.ReadFile(filepath.Join(logDir, lf))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		var fileIssues []logEntry
		for _, line := range lines {
			if line == "" {
				continue
			}
			issue, attrs := parseSlogEntry(line)
			if isUploadIssue(issue, attrs) {
				issue.File = lf
				fileIssues = append(fileIssues, issue)
			}
		}
		// Lines in a file are oldest first; the panel shows newest first.
		for i := len(fileIssues) - 1; i >= 0 && len(issues) < maxUploadIssues; i-- {
			issues = append(issues, fileIssues[i])
		}
		if len(issues) >= maxUploadIssues {
			break
		}
	}

	// Also check for DB records of failed uploads (photos with missing files)
	var missingPhotos []map[string]any
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT p.id, p.filename, p.original_name, p.trip_id, p.created_at, t.title as trip_title
		FROM photos p
		LEFT JOIN trips t ON t.id = p.trip_id
		ORDER BY p.created_at DESC
	`)
	if err == nil {
		for rows.Next() {
			var id, filename, origName, tripID, createdAt string
			var tripTitle sql.NullString
			if err := rows.Scan(&id, &filename, &origName, &tripID, &createdAt, &tripTitle); err != nil {
				continue
			}
			// Check if file exists on disk
			path := filepath.Join(s.UploadDir, filename)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				tt := ""
				if tripTitle.Valid {
					tt = tripTitle.String
				}
				missingPhotos = append(missingPhotos, map[string]any{
					"id":            id,
					"filename":      filename,
					"original_name": origName,
					"trip_id":       tripID,
					"trip_title":    tt,
					"created_at":    createdAt,
				})
			}
		}
		rows.Close()
	}

	jsonOK(w, map[string]any{
		"log_issues":     issues,
		"missing_photos": missingPhotos,
	})
}

// ---------------------------------------------------------------------------
// Admin API: Active sessions
// ---------------------------------------------------------------------------

func (s *Server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT token, user_id, email, created_at, expires_at,
		       CASE WHEN datetime(expires_at) > datetime('now') THEN 1 ELSE 0 END as active
		FROM sessions ORDER BY created_at DESC
	`)
	if err != nil {
		slog.Error("admin: list sessions", "error", err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type sessionInfo struct {
		Token     string `json:"token"`
		UserID    string `json:"user_id"`
		Email     string `json:"email"`
		CreatedAt string `json:"created_at"`
		ExpiresAt string `json:"expires_at"`
		Active    bool   `json:"active"`
	}

	var sessions []sessionInfo
	for rows.Next() {
		var s sessionInfo
		var active int
		if err := rows.Scan(&s.Token, &s.UserID, &s.Email, &s.CreatedAt, &s.ExpiresAt, &active); err != nil {
			continue
		}
		s.Active = active == 1
		// Mask token for security
		if len(s.Token) > 8 {
			s.Token = s.Token[:8] + "..."
		}
		sessions = append(sessions, s)
	}

	jsonOK(w, sessions)
}

// ---------------------------------------------------------------------------
// Admin API: Impersonate user
// ---------------------------------------------------------------------------

func (s *Server) handleAdminImpersonate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	var body struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.UserID == "" || body.Email == "" {
		jsonError(w, "user_id and email are required", http.StatusBadRequest)
		return
	}

	slog.Info("admin impersonate", "admin", s.AdminEmail, "target_email", body.Email, "target_user_id", body.UserID)

	http.SetCookie(w, &http.Cookie{
		Name:     impersonateCookieName,
		Value:    body.UserID + ":" + body.Email,
		Path:     "/",
		MaxAge:   3600, // 1 hour
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   proto(r) == "https",
	})

	jsonOK(w, map[string]any{"ok": true, "impersonating": body.Email})
}

func (s *Server) handleAdminStopImpersonate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	slog.Info("admin stop impersonation", "admin", s.AdminEmail)

	http.SetCookie(w, &http.Cookie{
		Name:     impersonateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   proto(r) == "https",
	})

	jsonOK(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Admin API: System info
// ---------------------------------------------------------------------------

func (s *Server) handleAdminSystem(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	hostname, _ := os.Hostname()
	var uptimeStr string
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) > 0 {
			if secs, err := strconv.ParseFloat(parts[0], 64); err == nil {
				d := time.Duration(secs * float64(time.Second))
				uptimeStr = d.Round(time.Second).String()
			}
		}
	}

	jsonOK(w, map[string]any{
		"hostname":   hostname,
		"uptime":     uptimeStr,
		"upload_dir": s.UploadDir,
		"now":        time.Now().UTC().Format(time.RFC3339),
	})
}
