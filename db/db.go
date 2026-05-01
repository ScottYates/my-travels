package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

//go:generate go tool github.com/sqlc-dev/sqlc/cmd/sqlc generate

//go:embed migrations/*.sql
var migrationFS embed.FS

// Open opens an sqlite database and prepares pragmas suitable for a small web app.
// It uses _time_format=sqlite so that time.Time values are stored in a standard
// format (YYYY-MM-DD HH:MM:SS.SSS±HH:MM) that the driver can reliably parse
// back into time.Time on reads.
func Open(path string) (*sql.DB, error) {
	v := url.Values{}
	v.Set("_time_format", "sqlite")
	v.Set("_txlock", "immediate")
	v.Add("_pragma", "foreign_keys(1)")
	v.Add("_pragma", "journal_mode(wal)")
	v.Add("_pragma", "busy_timeout(1000)")
	dsn := "file:" + path + "?" + v.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Normalize any timestamps previously stored in Go's default String()
	// format ("2006-01-02 15:04:05 -0700 -0700") to the sqlite format
	// ("2006-01-02 15:04:05-07:00") so the driver can parse them.
	if err := normalizeTimestamps(db); err != nil {
		slog.Warn("db: failed to normalize timestamps", "err", err)
	}

	return db, nil
}

// normalizeTimestamps rewrites timestamps stored in Go's default time.Time
// String() format to the standard SQLite format that the driver can parse.
// Go's format: "2006-01-02 15:04:05.999999999 -0700 -0700" or with "m=+..." suffix
// SQLite format: "2006-01-02 15:04:05.999999999-07:00"
func normalizeTimestamps(db *sql.DB) error {
	// Check if any tables exist yet (first run).
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('trips','stops','photos','comments','routes')").Scan(&n); err != nil || n == 0 {
		return nil
	}

	// Each entry: table, column, primary key column.
	cols := []struct{ table, col, pk string }{
		{"trips", "created_at", "id"},
		{"trips", "updated_at", "id"},
		{"stops", "arrived_at", "id"},
		{"stops", "created_at", "id"},
		{"photos", "taken_at", "id"},
		{"photos", "created_at", "id"},
		{"comments", "created_at", "id"},
		{"routes", "created_at", "id"},
	}

	// Go's time.Time.String() produces formats like:
	//   "2006-01-02 15:04:05.999999999 +0000 UTC m=+123.456"
	//   "2006-01-02 15:04:05.999999999 -0400 -0400"
	// We parse these and rewrite to the sqlite format.
	goFormats := []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 -0700",
		"2006-01-02 15:04:05.999999999 -0700 -0700",
	}
	const sqliteFmt = "2006-01-02 15:04:05.000000000-07:00"

	mRe := regexp.MustCompile(`\s+m=[+-]`)

	type fix struct {
		pk, val string
	}

	var totalFixed int
	for _, c := range cols {
		query := fmt.Sprintf(
			"SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND (%s LIKE '%%m=%%' OR %s LIKE '%% -____ -____' OR %s LIKE '%% +____ ____')",
			c.pk, c.col, c.table, c.col, c.col, c.col, c.col,
		)
		rows, err := db.Query(query)
		if err != nil {
			continue
		}
		var fixes []fix
		for rows.Next() {
			var pk, val string
			if err := rows.Scan(&pk, &val); err != nil {
				continue
			}
			// Strip m=+... suffix if present.
			s := val
			if idx := mRe.FindStringIndex(s); idx != nil {
				s = s[:idx[0]]
			}
			for _, f := range goFormats {
				if t, err := time.Parse(f, s); err == nil {
					fixes = append(fixes, fix{pk, t.Format(sqliteFmt)})
					break
				}
			}
		}
		rows.Close()

		for _, f := range fixes {
			upd := fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", c.table, c.col, c.pk)
			if _, err := db.Exec(upd, f.val, f.pk); err == nil {
				totalFixed++
			}
		}
	}

	if totalFixed > 0 {
		slog.Info("db: normalized timestamps", "count", totalFixed)
	}
	return nil
}

// RunMigrations executes database migrations in numeric order (NNN-*.sql),
// similar in spirit to exed's exedb.RunMigrations.
func RunMigrations(db *sql.DB) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var migrations []string
	pat := regexp.MustCompile(`^(\d{3})-.*\.sql$`)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if pat.MatchString(name) {
			migrations = append(migrations, name)
		}
	}
	sort.Strings(migrations)

	executed := make(map[int]bool)
	var tableName string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='migrations'").Scan(&tableName)
	switch {
	case err == nil:
		rows, err := db.Query("SELECT migration_number FROM migrations")
		if err != nil {
			return fmt.Errorf("query executed migrations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				return fmt.Errorf("scan migration number: %w", err)
			}
			executed[n] = true
		}
	case errors.Is(err, sql.ErrNoRows):
		slog.Info("db: migrations table not found; running all migrations")
	default:
		return fmt.Errorf("check migrations table: %w", err)
	}

	for _, m := range migrations {
		match := pat.FindStringSubmatch(m)
		if len(match) != 2 {
			return fmt.Errorf("invalid migration filename: %s", m)
		}
		n, err := strconv.Atoi(match[1])
		if err != nil {
			return fmt.Errorf("parse migration number %s: %w", m, err)
		}
		if executed[n] {
			continue
		}
		if err := executeMigration(db, m); err != nil {
			return fmt.Errorf("execute %s: %w", m, err)
		}
		slog.Info("db: applied migration", "file", m, "number", n)
	}
	return nil
}

func executeMigration(db *sql.DB, filename string) error {
	content, err := migrationFS.ReadFile("migrations/" + filename)
	if err != nil {
		return fmt.Errorf("read %s: %w", filename, err)
	}
	if _, err := db.Exec(string(content)); err != nil {
		return fmt.Errorf("exec %s: %w", filename, err)
	}
	return nil
}
