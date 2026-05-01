# My Travels

A self-hosted travel journal with an interactive 3D globe. Organize trips into stops, upload geotagged photos and videos, and view them pinned on a [CesiumJS](https://cesium.com/cesiumjs/) globe. Includes a presentation mode for sharing trips as full-screen slideshows and read-only share links for public viewing.

## Features

- **3D Globe** — CesiumJS globe with photo markers at GPS coordinates, route polylines between stops, and fly-to navigation
- **Trip / Stop / Photo management** — create trips, add ordered stops with locations, upload photos (EXIF GPS extraction) and videos
- **Presentation mode** — full-screen slideshow with captions, mini-globe, and keyboard navigation
- **Share links** — read-only URLs for sharing trips publicly (no login required)
- **Google OAuth login** — optional; the app works without it on exe.dev (uses `X-ExeDev-*` headers)
- **SQLite database** — zero-config, file-based storage with auto-migrations
- **Video support** — upload videos with ffmpeg-generated thumbnails

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
│       └── index.html       # Single-page app (HTML/CSS/JS, ~8000 lines)
├── db/
│   ├── db.go                # Database open, pragma config, migration runner
│   ├── migrations/          # Sequential SQL migrations (001–011)
│   ├── queries/             # SQL queries for sqlc code generation
│   ├── dbgen/               # Generated Go code from sqlc
│   └── sqlc.yaml            # sqlc configuration
├── uploads/                 # User-uploaded photos/videos (gitignored)
├── db.sqlite3               # SQLite database file (gitignored)
├── srv.service              # systemd unit file
├── Makefile                 # Build targets
├── go.mod / go.sum          # Go module files
└── AGENTS.md                # Agent/AI assistant instructions
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

The Makefile builds the binary to the parent directory (`../srv` relative to the source root). For local development you can build wherever you like:

```bash
# Default (builds to /home/exedev/srv for exe.dev deployment)
make build

# Or build locally
go build -o my-travels ./cmd/srv
```

### 4. Run

The server looks for `db.sqlite3` in the **working directory** and creates it automatically on first run. Migrations are applied automatically.

```bash
# Run from the directory where you want db.sqlite3 and uploads/ to live
./my-travels
```

The server starts on port **8000** by default. Open http://localhost:8000.

To use a different port:

```bash
./my-travels -listen :3000
```

### 5. (Optional) Configure Google OAuth

Google OAuth lets users log in and own their trips. Without it, the app still runs but has no authentication.

#### a. Create a Google Cloud OAuth client

1. Go to the [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
2. Create a project (or select an existing one)
3. Go to **APIs & Services → Credentials → Create Credentials → OAuth client ID**
4. Set **Application type** to **Web application**
5. Under **Authorized redirect URIs**, add the callback URL for each environment where you'll run the app:
   - Local development: `http://localhost:8000/auth/google/callback`
   - exe.dev: `https://<vm-name>.exe.xyz:8000/auth/google/callback`
   - Custom domain: `https://yourdomain.com/auth/google/callback`
6. Copy the **Client ID** and **Client Secret**

> The redirect URI must exactly match your deployment URL. The app builds it dynamically from the request origin + `/auth/google/callback`.

#### b. Set environment variables

For local development, export before starting the server:

```bash
export GOOGLE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export GOOGLE_CLIENT_SECRET="GOCSPX-your-secret"
./my-travels
```

For systemd deployment on exe.dev, create `/home/exedev/.env`:

```
GOOGLE_CLIENT_ID=your-client-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=GOCSPX-your-secret
```

The service unit loads this file automatically via `EnvironmentFile=-/home/exedev/.env`.

### 6. (Optional) Install ffmpeg for video support

Video uploads require `ffmpeg` and `ffprobe` for thumbnail generation:

```bash
# Debian/Ubuntu
sudo apt-get install -y ffmpeg

# macOS
brew install ffmpeg
```

## Deployment on exe.dev

### Build and install the service

```bash
cd /home/exedev/my-travels
make build                    # Builds binary to /home/exedev/srv
```

### Set up systemd

```bash
sudo cp srv.service /etc/systemd/system/srv.service
sudo systemctl daemon-reload
sudo systemctl enable --now srv
```

The service runs as `exedev`, reads `/home/exedev/.env` for environment variables, and serves on port 8000.

### Restart after code changes

```bash
make build
sudo systemctl restart srv
```

### View logs

```bash
journalctl -u srv -f
```

### Access

On exe.dev, the app is available at `https://<vm-name>.exe.xyz:8000/`.

## Database

SQLite with WAL mode, foreign keys enabled. The database file (`db.sqlite3`) is created automatically on first run.

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
| POST | `/api/trips` | Create a trip |
| GET | `/api/trips` | List user's trips |
| POST | `/api/trips/:id/stops` | Add a stop to a trip |
| POST | `/api/stops/:id/photos` | Upload photos to a stop |
| PUT | `/api/photos/:id` | Update photo metadata |
| DELETE | `/api/photos/:id` | Delete a photo |
| POST | `/api/trips/:id/routes` | Add a route between stops |
| GET | `/api/trips/:id/present` | Get presentation data |

## CesiumJS Globe

The app uses the free CesiumJS library with open tile providers (no Cesium Ion token required). Photo markers are rendered as circular billboard thumbnails on the globe at their GPS coordinates.

## License

Private project.
