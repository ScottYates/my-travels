package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"srv.exe.dev/srv"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Load .env from working directory (does not override existing env vars).
	loadEnvFile(".env")

	listenAddr := envDefault("LISTEN", ":8000")
	baseDir := envDefault("BASE_DIR", "")
	logDir := os.Getenv("LOG_DIR")
	googleClientID := os.Getenv("GOOGLE_CLIENT_ID")
	googleClientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")

	if baseDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("determine executable path: %w", err)
		}
		baseDir = filepath.Dir(exe)
	}

	// Set up logging: stderr + optional log file.
	if logDir != "" {
		if err := setupFileLogging(logDir); err != nil {
			return fmt.Errorf("setup logging: %w", err)
		}
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	server, err := srv.New("db.sqlite3", hostname, googleClientID, googleClientSecret, baseDir)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return server.Serve(listenAddr)
}

// setupFileLogging creates a log directory and configures slog to write
// to both stderr and a daily log file (logs/YYYY-MM-DD.log).
func setupFileLogging(logDir string) error {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log directory %s: %w", logDir, err)
	}

	logFile := filepath.Join(logDir, time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logFile, err)
	}

	// Write to both stderr and the log file.
	multi := io.MultiWriter(os.Stderr, f)
	handler := slog.NewTextHandler(multi, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))
	slog.Info("logging to file", "path", logFile)
	return nil
}

// envDefault returns the value of the environment variable named by key,
// or fallback if the variable is unset or empty.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadEnvFile reads a .env-style file and sets any variables that are not
// already present in the environment. Lines starting with # and blank lines
// are ignored. Values may optionally be quoted with single or double quotes.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // missing .env is fine
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		// Don't override existing env vars
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
