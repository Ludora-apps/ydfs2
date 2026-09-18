# Web UI: architecture, tech stack, installation

`webui/` is a self-contained Go + React application ("ydfs-web") that gives
the same [build process](build-process.md) a browser dashboard: pick a build
target and options, queue it, watch live logs, download artifacts — driven by
the same Docker Compose service and `2.12/scripts/` as the command line.

For the authoritative day-to-day instructions (build, deploy, API, tests) see
[`webui/README.md`](../webui/README.md); this page adds the "how it fits
together" view plus a condensed install path. Also see
[`webui/VALIDATION.md`](../webui/VALIDATION.md) (what was actually tested)
and [`webui/deploy/HOST.md`](../webui/deploy/HOST.md) (a real production
deployment record).

## Tech stack

| Layer | Technology |
| --- | --- |
| Backend | Go 1.25+, standard `net/http`, no web framework |
| Persistence | SQLite via `modernc.org/sqlite` (pure Go, no cgo) |
| Build execution | Shells out to `docker compose` (the repo's `2.12/` service) and `docker` (logs/cancel/inspect) |
| Frontend | React 19 + TypeScript, built with Vite 7 |
| Frontend tests | Playwright (Chromium) |
| Backend tests | Go `testing` + `-race`; a real-Docker fixture behind `YDFS_DOCKER_TEST=1` |
| Packaging | The compiled Go binary **embeds** the built frontend (`go:embed` in `webui/internal/assets/assets.go`, reading `webui/internal/assets/dist/`) — a single static executable, no Node.js needed at runtime |
| Reverse proxy / auth (production) | Caddy or Nginx (templates provided for both) + OAuth2 Proxy (GitHub OAuth) |
| Process supervision (production) | systemd unit files (`webui/deploy/*.service`) |
| CI | GitHub Actions, [`.github/workflows/webui.yml`](../.github/workflows/webui.yml) — builds, `go vet`, race tests, the Docker lifecycle fixture, and Playwright, on every push/PR touching `webui/**` |

### Source layout

```
webui/
├── main.go                 CLI flags, HTTP server wiring, auth middleware
├── model.go                Job/Profile/Settings/Artifact types + SQLite schema
├── runner.go                Docker Compose process management, log streaming, disk checks
├── main_test.go, docker_test.go   Go tests (docker_test.go = real-Docker fixture)
├── internal/assets/         go:embed wrapper around the built UI (dist/)
├── deploy/                  Caddyfile, nginx template, oauth2-proxy config, systemd units, HOST.md
└── ui/                      React/TypeScript frontend (Vite project)
    ├── src/main.tsx, style.css
    ├── tests/builds.spec.ts   Playwright end-to-end tests
    └── vite.config.ts, tsconfig.json
```

### How a job runs

1. The browser submits `Settings` (`target`, `verbose`, `kernel`) via
   `POST /api/jobs`.
2. The server snapshots the current `2.12/` working tree (including local
   uncommitted changes, excluding `config.ini`) into the job's data
   directory, records the Git revision/tag, and saves `working-tree.patch`.
3. It launches the `ydfs2.12` Compose service (`docker compose run`,
   equivalent to the `Makefile-docker` targets described in
   [build-process.md](build-process.md)) against the snapshot, copied to
   `/tmp/ydfs` inside the container.
4. Container stdout/stderr is streamed to the browser over Server-Sent
   Events (`GET /api/jobs/{id}/events`) and persisted so a server restart can
   replay history and resume monitoring a still-running container.
5. On completion, exit code and expected output files are checked; artifacts
   become downloadable via `GET /api/jobs/{id}/artifacts/{name}`.

Only one job runs at a time (persistent FIFO queue backed by SQLite). See
`webui/README.md`'s "Build behavior" section for the full list of guarantees
and caveats (cache reuse, cancellation semantics, disk-space admission
checks, etc.) — that section is intentionally not duplicated here.

## Installation — local / development

Requirements: Linux x86_64, Go 1.25+, Node.js 22.12+, npm, Git, Docker Engine
+ Docker Compose, and Docker access for the account running the server. The
checkout must include `2.12/` and its Git metadata (used to record
revision/tag per job).

```sh
cd webui
make build                 # npm ci && npm run build (ui/), then go build → ./ydfs-web
./ydfs-web --repo .. --dev --port 8080
```

Open `http://127.0.0.1:8080`. `--dev` disables authentication and refuses to
bind anything but a loopback address — never expose it through a proxy.

Frontend-only iteration with hot reload:

```sh
cd webui/ui
npm run dev                 # Vite dev server on 127.0.0.1:5173, proxies /api to :8080
```

CLI flags (`webui/main.go`):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--repo` | `..` | Path to the repository checkout to build from |
| `--data` | `~/.local/share/ydfs-web` | Persistent storage: SQLite DB, snapshots, logs, artifacts |
| `--port` | `8080` | HTTP port |
| `--host` | `127.0.0.1` | Listen address |
| `--origin` | *(none)* | Public origin (e.g. `https://build.example.org`) — required in non-dev mode for CSRF-style origin checks |
| `--dev` | `false` | Disable authentication; forces loopback-only |
| `--min-free-gb` | `10` | Minimum free disk space required to admit/start a build |

Keep at least the configured free space available; real distro builds may
need substantially more than the default 10 GiB. The dashboard surfaces
current free space and enforces the threshold before queueing and before
starting queued work.

## Installation — internet-facing / team deployment

Full step-by-step instructions (GitHub OAuth App setup, secret generation,
systemd units, Caddy vs. existing-Nginx choice) are in the "Internet/team
deployment" section of [`webui/README.md`](../webui/README.md) — follow that
document directly rather than a paraphrase here, since it is the maintained
source of truth and includes exact file paths/permissions. In short, the
request path is:

```text
Browser → Caddy or Nginx HTTPS on 0.0.0.0:8088 → OAuth2 Proxy (GitHub) → Go on 127.0.0.1:8080
```

`webui/deploy/HOST.md` documents one concrete production instance
(`linuxconsole.ludora.studio`) as a worked example of the same steps,
including the redeploy command (`install` the rebuilt binary +
`systemctl restart ydfs-web`) and where to check logs.

## Testing

```sh
cd webui
make test                                              # go vet-equivalent unit/integration tests
YDFS_DOCKER_TEST=1 go test -race -run TestDockerLifecycle -v .   # real Docker container lifecycle (small Alpine image)
cd ui && npx playwright install chromium && npm test   # UI end-to-end tests
```

A real fast-ISO smoke build (actually compiling packages) is deliberately
kept out of the automated suite — it downloads substantial data and takes a
long time. Run it through the UI itself and verify the resulting ISO boots
before distributing it; see `VALIDATION.md` for the last recorded smoke run
and its SHA256-verified artifact download.

## Build logs

All build logs live together in `DATA/logs-build/<job id>.log` rather than
inside each job's directory, so they outlive the artifacts they describe. The
**Build logs** box lists them; opening one gives a full-page reader with
scrolling and a text search that highlights every match and steps through them.
Each log can be downloaded or deleted on its own, and deleting a build always
deletes its log. Logs from earlier versions are migrated at startup.

The same reader shows a build's archived `config.ini` — the configuration the
build actually ran with — from the build details box and from **Kept builds**.
It opens in the page rather than a browser tab, sized to its content, and is
searchable and downloadable like a log.

## Keeping build artifacts

Artifacts are on a rolling window of 3, counted separately for ISOs and for
component builds. When a build ages out, only its artifacts are deleted — an
ISO is ~3.3 GB against ~70 MB for the rest of a job directory — so the build
stays listed with its log and its `config.ini` still readable. A build can be
pinned ("Keep"), which exempts its artifacts permanently and lists it in the
separate **Kept builds** box, where it can be downloaded, released or deleted
and its configuration read. Releasing it makes it eligible again immediately.

## Pre-installing Flathub applications

The build form carries a **Flathub applications** box for ISO targets: the
twenty most popular Flathub applications, fetched live from Flathub and cached
server-side. Ticked applications are downloaded during the build and baked into
the ISO. The endpoints are `GET /api/flathub` (cached, no network call) and
`POST /api/flathub/refresh`. See
[flatpak-preinstall.md](flatpak-preinstall.md).

## API surface

`GET /api/capabilities`, `GET/POST /api/jobs`, `GET/DELETE /api/jobs/{id}`,
`POST /api/jobs/{id}/cancel`, `GET /api/jobs/{id}/events` (SSE log stream,
`Last-Event-ID`/`?offset=` to resume), `GET /api/jobs/{id}/log`,
`GET /api/jobs/{id}/artifacts/{name}`, `GET/PUT /api/profiles`,
`DELETE /api/profiles/{name}`. `Settings` shape:
`{ "target": "fast-iso", "verbose": true, "kernel": "" }`. See
`webui/README.md` for the `Profile` shape and SSE event details.
