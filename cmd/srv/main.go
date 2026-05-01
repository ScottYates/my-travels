package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"srv.exe.dev/srv"
)

var (
	flagListenAddr = flag.String("listen", ":8000", "address to listen on")
	flagBaseDir    = flag.String("base-dir", "", "base directory containing srv/templates, srv/static, and uploads (default: directory of executable)")
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	baseDir := *flagBaseDir
	if baseDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("determine executable path: %w", err)
		}
		baseDir = filepath.Dir(exe)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	googleClientID := os.Getenv("GOOGLE_CLIENT_ID")
	googleClientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	server, err := srv.New("db.sqlite3", hostname, googleClientID, googleClientSecret, baseDir)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return server.Serve(*flagListenAddr)
}
