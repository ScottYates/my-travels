# My Travels

A self-hosted travel journal with an interactive 3D globe. Organize trips into stops, upload geotagged photos and videos, and view them pinned on a [CesiumJS](https://cesium.com/cesiumjs/) globe. Includes a presentation mode for sharing trips as full-screen slideshows and read-only share links for public viewing.

## Features

- **3D Globe** — CesiumJS globe with photo markers at GPS coordinates, route polylines between stops, and fly-to navigation
- **Trip / Stop / Photo management** — create trips, add ordered stops with locations, upload photos (EXIF GPS extraction) and videos
- **Presentation mode** — full-screen slideshow with captions, mini-globe, and keyboard navigation
- **Share links** — read-only URLs for sharing trips publicly (no login required)
- **Google OAuth login** — optional authentication for multi-user setups
- **SQLite database** — zero-config, file-based storage with auto-migrations
- **Video support** — upload videos with ffmpeg-generated thumbnails
- **Structured logging** — all HTTP requests and key operations logged via Go's `slog`

## Prerequisites

- **Go 1.22+** (tested with Go 1.26)
- **SQLite** (bundled via `modernc.org/sqlite`, no CGO required)
- **ffmpeg / ffprobe** (optional, for video thumbnail generation)

## Project Structure

```
my-travels/
├── cmd/srv/main.go          # Entry point — parses flags, starts server
├── srv/
│   ├── server.go            # HTTP handlers, routes, business logic
│   ├── google_auth.go       # Google OAuth 2.0 flow
│   ├── resort_photos.go     # Photo reordering logic
│   ├── server_test.go       # Tests
│   ├── static/              # Static assets (CSS, JS)
│   └── templates/
│       └── index.html       # Single-page app (HTML/CSS/JS)
├── db/
│   ├── db.go                # Database open, pragma config, migration runner
│   ├── migrations/          # Sequential SQL migrations (001–011)
│   ├── queries/             # SQL queries for sqlc code generation
│   ├── dbgen/               # Generated Go code from sqlc
│   └── sqlc.yaml            # sqlc configuration
├── uploads/                 # User-uploaded photos/videos (gitignored)
├── db.sqlite3               # SQLite database file (gitignored, created on first run)
├── srv.service              # systemd unit file
├── Makefile                 # Build targets
└── go.mod / go.sum          # Go module files
```

## Quick Start

### 1. Clone the repository

```bash
git clone https://github.com/ScottYates/my-travels.git
cd my-travels
```

### 2. Install Go dependencies

```bash
go mod download
```

### 3. Build

```bash
make build
```

This produces a `./my-travels` binary in the repo root. To build to a custom path:

```bash
make build OUT=/usr/local/bin/my-travels
```

### 4. Run

Run from the repo root so the server can find `srv/templates/` and `srv/static/`:

```bash
./my-travels
```

The server starts on port **8000** by default. Open http://localhost:8000.

On first run it will:
- Create `db.sqlite3` in the working directory
- Apply all database migrations automatically
- Create the `uploads/` directory for photo storage

To use a different port:

```bash
./my-travels -listen :3000
```

#### Running the binary from a different location

If the binary is not in the repo root (e.g. installed to `/usr/local/bin`), tell it where to find the source tree:

```bash
my-travels -base-dir /path/to/my-travels
```

The `-base-dir` flag sets the root directory for finding `srv/templates/`, `srv/static/`, and `uploads/`. It defaults to the directory containing the executable.

### 5. (Optional) Configure Google OAuth

Google OAuth lets users log in and own their trips. Without it, the app still runs but has no authentication.

#### a. Create a Google Cloud OAuth client

1. Go to the [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
2. Create a project (or select an existing one)
3. Go to **APIs & Services → Credentials → Create Credentials → OAuth client ID**
4. Set **Application type** to **Web application**
5. Under **Authorized redirect URIs**, add the callback URL for each environment where you'll run the app:
   - Local development: `http://localhost:8000/auth/google/callback`
   - Production: `https://yourdomain.com/auth/google/callback`
6. Copy the **Client ID** and **Client Secret**

> The redirect URI must exactly match your deployment URL. The app builds it dynamically from the request origin + `/auth/google/callback`.

#### b. Set environment variables

Export before starting the server:

```bash
export GOOGLE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export GOOGLE_CLIENT_SECRET="GOCSPX-your-secret"
./my-travels
```

Or create a `.env` file for use with the systemd service (see below):

```
GOOGLE_CLIENT_ID=your-client-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=GOCSPX-your-secret
```

### 6. (Optional) Install ffmpeg for video support

Video uploads require `ffmpeg` and `ffprobe` for thumbnail generation:

```bash
# Debian/Ubuntu
sudo apt-get install -y ffmpeg

# macOS
brew install ffmpeg
```

## Deployment with systemd

The included `srv.service` file runs the server as a systemd service. Edit it to match your paths.

### Install and start

```bash
# Edit srv.service: set WorkingDirectory, ExecStart path, and -base-dir
sudo cp srv.service /etc/systemd/system/srv.service
sudo systemctl daemon-reload
sudo systemctl enable --now srv
```

The service reads environment variables from `~/.env` (via `EnvironmentFile`).

### Restart after code changes

```bash
make build OUT=/path/to/binary
sudo systemctl restart srv
```

### View logs

```bash
journalctl -u srv -f
```

## Logging

The server uses Go's structured `slog` package. All output goes to stderr (captured by systemd journal or your terminal).

### Startup logs

On startup the server logs its resolved configuration:

```
INFO server init base_dir=/path/to/my-travels upload_dir=.../uploads templates_dir=.../srv/templates static_dir=.../srv/static go_version=go1.26.2
INFO starting server addr=:8000
```

### HTTP request logs

Every request is logged with method, path, status code, response size, duration, and remote address:

```
INFO http request method=GET path=/ status=200 bytes=276939 duration=108ms remote=[::1]:48910
WARN http request method=GET path=/api/trips status=401 bytes=28 duration=0ms remote=192.168.1.5:52300
ERROR http request method=POST path=/api/trips/x/photos status=500 bytes=42 duration=15ms remote=...
```

- **INFO** — 2xx and 3xx responses
- **WARN** — 4xx responses (client errors)
- **ERROR** — 5xx responses (server errors)

### Upload logs

Photo and video uploads are logged at each stage:

```
INFO upload: receiving file original_name=IMG_1234.jpg size=4500000 trip_id=abc-123
INFO upload: saved file path=/path/to/uploads/uuid.jpg bytes=4500000
INFO upload: photo created id=uuid filename=uuid.jpg lat=39.916 lng=116.397
```

Upload failures include the error detail:

```
ERROR upload: write file to disk error="permission denied" path=/path/to/uploads/uuid.jpg
```

### Database migration logs

```
INFO db: applied migration file=001-base.sql number=1
```

## Database

SQLite with WAL mode, foreign keys enabled. The database file (`db.sqlite3`) is created automatically in the working directory on first run.

### Migrations

Migrations live in `db/migrations/` and are applied automatically on startup. They follow the pattern `NNN-name.sql` and are tracked in a `migrations` table.

### sqlc Code Generation

SQL queries in `db/queries/` are compiled to Go code using [sqlc](https://sqlc.dev/):

```bash
cd db
go tool github.com/sqlc-dev/sqlc/cmd/sqlc generate
```

Generated code goes to `db/dbgen/`. The sqlc tool is declared in `go.mod` so no separate install is needed.

## API Overview

All data is managed through JSON REST endpoints. Key routes:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | Main single-page app |
| GET | `/share/:slug` | Read-only shared trip view |
| GET | `/present/:slug` | Presentation mode |
| POST | `/api/trips` | Create a trip |
| GET | `/api/trips` | List user's trips |
| POST | `/api/trips/:id/stops` | Add a stop to a trip |
| POST | `/api/trips/:id/photos` | Upload photos to a stop |
| PUT | `/api/photos/:id` | Update photo metadata |
| DELETE | `/api/photos/:id` | Delete a photo |
| POST | `/api/trips/:id/routes` | Add a route between stops |

## Troubleshooting

### Uploads fail on a new install

The most common cause is a **wrong base directory**. The server resolves `uploads/` relative to `-base-dir` (default: the executable's directory). If the binary is somewhere else, it may try to write to a directory it can't access.

**Fix:** Pass `-base-dir` pointing at the repo root:

```bash
./my-travels -base-dir /path/to/my-travels
```

Check the startup log to verify paths:

```
INFO server init base_dir=... upload_dir=... templates_dir=... static_dir=...
```

### Templates not found

Same root cause — the server looks for `srv/templates/` inside the base directory. If the directory doesn't exist, the server exits immediately with a clear error:

```
templates directory not found at /some/path/srv/templates — set -base-dir to the project root
```

### Google OAuth not working

Check that:
1. `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET` environment variables are set
2. The **Authorized redirect URI** in Google Cloud Console exactly matches your server's origin + `/auth/google/callback`
3. The OAuth consent screen is configured (at minimum: app name and user support email)

## CesiumJS Globe

The app uses the free CesiumJS library with open tile providers (no Cesium Ion token required). Photo markers are rendered as circular billboard thumbnails on the globe at their GPS coordinates.

## License

Private project.
