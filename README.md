# MineDash

A self-hosted Minecraft server management sidecar. MineDash runs as a Docker
container alongside your `itzg/minecraft-server` instance and gives you
a clean web interface to start, stop, and restart the server, and to safely
download and upload the world as a ZIP file.

## Features

- **Server control** — start, stop, and restart the Minecraft container via the
  Docker socket; no SSH or RCON required
- **World download** — stream the world folder as a timestamped ZIP
  (`world-2026-03-12T09-00-00.zip`)
- **World upload** — upload a ZIP that atomically replaces the world; the
  previous world is kept as `world.old` as a safety net
- **Safe guards** — download and upload are only allowed while the server is
  stopped, preventing world corruption
- **Public status page** — the landing page shows live server status without login, with a session-aware Login or Dashboard link. Server controls and world management are available at `/dashboard` after signing in at `/login`.
- **Auth** — password-protected login backed by bcrypt + a signed JWT cookie;
  no database required
- **Single binary** — Go backend with the UI embedded at compile time; the
  Docker image is ~14 MB

- **Backup list** — browse dated archives newest first, with timestamps, sizes, and total storage. The Backups category appears only when matching files exist.

## Screenshots

![Dashboard](dashboard.png)
![Login](login.png)

## Requirements

- Docker with the Docker socket accessible (`/var/run/docker.sock`)
- A running `itzg/minecraft-server` container sharing a named volume
  with MineDash
- A reverse proxy (e.g. Traefik) — or expose port `8080` directly

## Quick start

### 1. Generate a password hash

The binary includes a built-in password hash generator — no third-party tools
needed:

```bash
# interactive (input is hidden)
./minedash genpw

# or without building first
go run . genpw
```

Copy the printed `$2a$12$…` hash for the next step.

> **Note:** bcrypt hashes contain `$` signs. In `.env` files and Docker Compose
> `environment:` blocks, each `$` must be escaped as `$$` to prevent variable
> interpolation — e.g. `$$2a$$12$$…`.

### 2. Create your `.env`

```bash
cp .env.example .env
```

Edit `.env`:

```dotenv
MC_STACK_NAME=${COMPOSE_PROJECT_NAME}  # set automatically by Compose
MC_SERVICE_NAME=minecraft              # Compose service name of the MC container
AUTH_PASSWORD_HASH=$$2a$$12$$...       # hash from step 1 — escape each $ as $$
JWT_SECRET=                            # openssl rand -hex 32
```

> If you run MineDash outside Docker Compose, use `MC_CONTAINER_NAME=<name>` instead of
> `MC_STACK_NAME`/`MC_SERVICE_NAME`.

### 3. Create your `compose.yml`

```bash
cp compose.example.yml compose.yml
```

Edit the Traefik hostname and any other values to match your setup.

### 4. Start the stack

```bash
docker compose up -d --build
```

Open `https://mc.example.com` in your browser, log in with the password you
chose in step 1, and you're done.

## Configuration

All configuration is done through environment variables. A `.env` file in the
project root is loaded automatically if present; variables already set in the
environment always take precedence.

| Variable | Required | Default | Description |
|---|---|---|---|
| `AUTH_PASSWORD_HASH` | yes | — | bcrypt hash (cost 12) of the login password; generate with `./minedash genpw` |
| `JWT_SECRET` | yes | — | HMAC key for signing session tokens; generate with `openssl rand -hex 32` |
| `MC_STACK_NAME` | yes* | — | Docker Compose project name; set to `${COMPOSE_PROJECT_NAME}` in compose.yml |
| `MC_SERVICE_NAME` | no | `minecraft` | Compose service name of the Minecraft container |
| `MC_CONTAINER_NAME` | no | — | Direct container name/ID override — bypasses label lookup; use outside Docker Compose |
| `MC_UID` | no | `1000` | UID to assign to the world folder after upload; must match `PUID` in the itzg container |
| `MC_GID` | no | `1000` | GID to assign to the world folder after upload; must match `PGID` in the itzg container |
| `PORT` | no | `8080` | Internal HTTP listen port |
| `BACKUP_PATH` | no | `/backups` | Directory containing mounted backup archives |
| `BACKUP_PREFIX` | no | `world` | Filename prefix for dated `.tar.gz` backups |
| `WORLD_PATH` | no | auto-detect | Explicit path to the world folder; skips scanning `/mc-data` for `level.dat` |

\* Required unless `MC_CONTAINER_NAME` is set.

### Container resolution

MineDash locates the Minecraft container using the following priority order:

1. **`MC_CONTAINER_NAME`** — used directly as the container name/ID; all label-based
   lookup is skipped.
2. **`MC_STACK_NAME` + `MC_SERVICE_NAME`** — the container is found by its Docker
   Compose labels (`com.docker.compose.project` / `com.docker.compose.service`).

When running inside Docker Compose, set `MC_STACK_NAME=${COMPOSE_PROJECT_NAME}` and
Compose fills in the project name automatically. Use `MC_CONTAINER_NAME` only when
running MineDash outside a Compose stack.

## Docker Compose layout

```
┌─────────────────────────────────────────────────────┐
│                  Docker host                        │
│                                                     │
│  ┌──────────────────┐    ┌──────────────────────┐   │
│  │   minecraft      │    │   minedash           │   │
│  │  itzg/mc-server  │    │   (this app)         │   │
│  │                  │    │                      │   │
│  │  /data  ─────────┼────┼── /mc-data           │   │
│  └──────────────────┘    └──────────┬───────────┘   │
│                                     │               │
│  /var/run/docker.sock ──────────────┘               │
└─────────────────────────────────────────────────────┘
```

- `mc-data` is a named Docker volume mounted as `/data` in the Minecraft
  container and as `/mc-data` in MineDash.
- MineDash auto-detects the world folder by scanning `/mc-data` for
  `level.dat`. Set `WORLD_PATH` to skip detection.
- The Docker socket gives MineDash control over the Minecraft container
  lifecycle without needing SSH or RCON.

## World management

### Download

1. Stop the server (or it is already stopped)
2. Click **Download World** — your browser downloads
   `world-<timestamp>.zip`

### Upload

1. Stop the server
2. Drag a world ZIP onto the upload area or click to select a file
3. Click **Upload & Replace World**

The upload endpoint:

1. Refuses the request if the Minecraft container is not stopped
2. Validates that the ZIP contains `level.dat`
3. Renames the current world to `world.old` (overwrites the previous backup)
4. Atomically moves the new world into place via `os.Rename`

### Recovering from a bad upload

If you need to roll back, stop the server and swap `world.old` back manually:

```bash
docker run --rm -v mc-data:/mc-data alpine sh -c \
  "rm -rf /mc-data/world && mv /mc-data/world.old /mc-data/world"
```

## Backups

The dashboard has **Status**, **World Management**, and an optional **Backups**
category. To enable the backup list, mount your backup directory into MineDash
read-only and configure the filename prefix. For example, add these entries to
the `minedash` service in your Compose file (alongside its existing entries):

```yaml
volumes:
  - /home/andreas/grassblock/backups:/backups:ro
environment:
  - BACKUP_PREFIX=grassblock
```

Only regular files named `<prefix>-YYYY-MM-DDTHH-MM-SS.tar.gz` are listed.
`grassblock.latest.tar.gz`, symlinks, directories, and unrelated files are ignored.
The category stays hidden if the mount is missing or has no matching backups.
The list refreshes automatically every 30 seconds. Dates and times come from
filenames and are displayed without timezone conversion. MineDash reads only
file metadata for the list. Each backup has a **Download** action that streams
the original `.tar.gz` archive to your browser. Downloads require login and are
available while the Minecraft server is running or stopped. Large downloads
support HTTP range requests for resuming transfers.

Each backup also has a **Restore** action, available only while the server is
confirmed stopped. Its native confirmation dialog warns that the entire mounted
server volume will be replaced: world, settings, mods, server files, and logs.
Archives must contain entries directly at the volume root. Restore uses the
shared writable `/mc-data` mount, independently of `WORLD_PATH`. Only regular
files and directories are supported; links, special files, unsafe paths, and
entries under the reserved `.minedash-restore` directory are rejected.

Restore stages the extracted backup alongside the current data, so sufficient
free disk space is required for both. MineDash blocks Start, Restart, world
upload, and additional restores throughout the operation. The server remains
stopped afterward. A persistent journal and rollback data in
`/mc-data/.minedash-restore` allow interrupted replacements to roll back on
MineDash startup. If recovery cannot finish, startup remains blocked; retain
that directory for recovery. Individual file moves are atomic, but the full
volume replacement is not. Starting the container directly through Docker
bypasses MineDash's guard. Run only one MineDash instance per server volume.

MineDash does not delete or create backup archives.
Set `BACKUP_PATH` if you use a different internal mount point.

Categories can be linked directly with `/dashboard#status`, `/dashboard#world`,
and `/dashboard#backups`. Refreshing preserves the selected category, and browser
Back/Forward navigates between selections. Missing or invalid fragments select
Status; a Backups link also falls back to Status when no backups are available.

## Building from source

```bash
git clone https://codeberg.org/apfohl/minedash
cd minedash
go build -o minedash .
```

Requires Go 1.26 or later.

## Tech stack

| Layer | Choice |
|---|---|
| Backend | Go 1.26, standard library `net/http` |
| Docker control | Docker SDK for Go (`github.com/docker/docker`) |
| Auth | bcrypt (`golang.org/x/crypto`) + JWT (`github.com/golang-jwt/jwt/v5`) |
| Frontend | Vanilla HTML/JS, embedded via `embed.FS` |
| Config | Environment variables via `github.com/joho/godotenv` |

## License

MIT — see [LICENSE](LICENSE).
