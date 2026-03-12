# MineDash

A self-hosted Minecraft server management sidecar. MineDash runs as a Docker
container alongside your `itzg/docker-minecraft-server` instance and gives you
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
- **Auth** — password-protected login backed by bcrypt + a signed JWT cookie;
  no database required
- **Single binary** — Go backend with the UI embedded at compile time; the
  Docker image is ~14 MB

## Screenshots

> Login page and dashboard with Minecraft-green dark theme.

## Requirements

- Docker with the Docker socket accessible (`/var/run/docker.sock`)
- A running `itzg/docker-minecraft-server` container sharing a named volume
  with MineDash
- A reverse proxy (e.g. Traefik) — or expose port `8080` directly

## Quick start

### 1. Generate a password hash

The binary includes a built-in password hash generator — no third-party tools
needed:

```bash
# interactive (input is hidden)
./minedash genpw

# or directly from the Docker image before the stack is up
docker run --rm -it codeberg.org/apfohl/minedash ./minedash genpw
```

Copy the printed `$2a$12$…` hash for the next step.

### 2. Create your `.env`

```bash
cp .env.example .env
```

Edit `.env`:

```dotenv
MC_CONTAINER_NAME=minecraft
AUTH_PASSWORD_HASH=$2a$12$...   # hash from step 1
JWT_SECRET=                     # openssl rand -hex 32
PORT=8080
```

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
| `AUTH_PASSWORD_HASH` | yes | — | bcrypt hash (cost 12) of the login password |
| `JWT_SECRET` | yes | — | HMAC key for signing session tokens (`openssl rand -hex 32`) |
| `MC_CONTAINER_NAME` | no | `minecraft` | Name of the Minecraft Docker container |
| `PORT` | no | `8080` | Internal HTTP listen port |
| `WORLD_PATH` | no | auto-detect | Explicit path to the world folder; skips scanning `/mc-data` for `level.dat` |

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
