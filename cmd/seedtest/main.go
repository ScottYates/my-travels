// seedtest is a small helper used by the Playwright end-to-end suite to seed
// a known session row into the SQLite database that the running Go server is
// using. The session token is then injected into the browser via Playwright's
// context.addCookies().
//
// This is intentionally a separate binary from cmd/srv. It uses the same
// dbgen package so the schema is guaranteed to match.
//
// Usage:
//
//	seedtest -db path/to/db.sqlite3 -user user-1 -email alice@example.com
//	seedtest -db ... -user admin-1 -email admin@test.com -admin
//
// Prints the resulting session token to stdout. Exit code is non-zero on
// error.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"srv.exe.dev/db"
	"srv.exe.dev/db/dbgen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seedtest:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "db.sqlite3", "path to sqlite database file")
	userID := flag.String("user", "", "user_id (required)")
	email := flag.String("email", "", "email (required)")
	hours := flag.Int("hours", 24, "session lifetime in hours")
	flag.Parse()
	if *userID == "" || *email == "" {
		flag.Usage()
		return fmt.Errorf("-user and -email are required")
	}

	wdb, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer wdb.Close()

	q := dbgen.New(wdb)
	// Delete any prior session for this user so the test starts clean.
	_, _ = wdb.ExecContext(context.Background(), `DELETE FROM sessions WHERE user_id = ?`, *userID)

	token, err := newToken()
	if err != nil {
		return err
	}
	now := time.Now()
	if err := q.CreateSession(context.Background(), dbgen.CreateSessionParams{
		Token:     token,
		UserID:    *userID,
		Email:     *email,
		CreatedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: now.Add(time.Duration(*hours) * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// The Playwright script reads stdout for the token.
	fmt.Println(token)
	return nil
}

// newToken returns a 32-character hex string. Server-issued tokens are UUIDs
// (36 chars) but the test only needs a unique opaque value.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
