# LinuxConsole web build manager

Go serves an embedded React/TypeScript interface and runs the existing LinuxConsole build scripts in Docker. A persistent queue runs one job at a time. All authenticated operators can configure, queue, cancel, view, download, and delete builds.

## Build and run locally

Requirements: Linux x86_64, Go 1.25 or newer, Node.js 22.12 or newer, npm, Git, Docker Engine, and Docker Compose. The account running the server needs Docker access. The checkout must contain `2.12/` and its Git metadata. A release tag is required for fast builds.

```sh
cd webui
make build
./ydfs-web --repo .. --dev --port 8080
```

Open http://127.0.0.1:8080. `--dev` disables authentication and only permits a loopback listener. For UI development, run `npm run dev` in `webui/ui` and use `--origin http://localhost:5173` on the development server. Vite proxies the API to port 8080.

The compiled executable embeds the UI; Node.js is not needed at runtime. Default storage is `~/.local/share/ydfs-web`, outside the checkout. Override it with `--data /path/to/storage`. Keep at least 10 GiB free (configurable with `--min-free-gb`); real distro builds may need substantially more. The dashboard reports available disk space. The manager checks the threshold before admission and before starting queued work.

## Build behavior

- Fast/full ISO, kernel, BusyBox, initramfs, update module, and Mate/KDE/Cinnamon presets use the `ydfs2.12` Compose service and its configured image. Compose pulls a missing image when starting the first build.
- Architecture is x86_64 and distribution is linuxconsole. Kernel versions must be explicit three-part numeric versions, and are configurable for full ISO/kernel builds only. Other defaults come from `scripts/make_config_ini` in the submitted source snapshot.
- Each submission snapshots the current `2.12` working tree, including local changes, excluding its `config.ini`. The server records the Git revision/tag and saves `working-tree.patch`. Files are copied before queueing, so later edits do not affect queued work. Do not edit the checkout during the brief snapshot operation.
- A submission may include `configOverrides`: extra `NAME=value` lines (one per line, optionally `NAME="value with spaces"`) appended to the generated `config.ini` before the build. Values may only contain letters, digits, and `` _./:+-``; `ARCH`, `DISTRONAME`, `BUILDYDFS`, `ISOTMP`, `SEND_BUILD_LOG`, `SEND_OPKG`, and `MENUCONFIG` are fixed afterward by the build manager and cannot be overridden. `GET /api/packages` lists the repository's `packages/list-*` files and `GET /api/packages/{name}` returns one's current content; a submission may set `packageList`/`packageListText` to replace that file inside its own snapshot only — the shared checkout and other queued or running jobs are unaffected.
- Containers copy the snapshot to `/tmp/ydfs`, generate configuration noninteractively, and compile there. The original checkout is not mounted writable. The generated configuration is retained in the job output directory.
- Verbose output is enabled by default. Turning it off follows the repository’s summary logging behavior; detailed package logs then remain in the build cache, not the browser log. Remote build-log/package uploads and terminal menuconfig are disabled for web jobs.
- The page header carries a repository box: the checkout's current commit, the latest commit on `https://github.com/linuxconsole-org/ydfs2` for the same branch, and an update button. Checking fetches that branch (and its tags) into `FETCH_HEAD` without touching the working tree. Updating fast-forwards a checkout that carries no commits of its own, and merges otherwise — a fork holding local work stays permanently diverged, which is expected; a conflicting merge is rolled back with the conflicting paths reported. Updating is refused while the working tree is dirty. The upstream URL is fixed in `repo.go` and is never read from the checkout's remotes, so it keeps tracking the original project even when `origin` points at a fork. Builds already queued or running keep the snapshot they were submitted with.
- Cache directories live under `DATA/build-home`, separate from ordinary command-line builds. They follow the repository’s incremental build behavior. A full build can reuse cached outputs. The runner prepares these directories with the same writable permissions needed by the repository’s container user; run this service on a host trusted by its build operators.
- ISO jobs have private output directories. Component outputs are copied out of shared caches before another job starts. Update jobs run `make updates` followed by `make_module linuxconsole` to produce the update module. Initramfs jobs build BusyBox first.
- Completion checks both exit status and nonempty expected outputs. This detects missing artifacts, but does not prove every upstream compiler invocation succeeded: some legacy scripts suppress errors. The manager does not claim a complete distro build is correct without inspecting/testing its output.
- Closing the browser does not stop work. Restarting the server resumes monitoring named Docker containers and replays persisted Docker logs. Docker containers must retain their log history for recovery. Do not run external Docker cleanup against active jobs.
- Cancellation stops the named container, including its child compilation processes. If Docker becomes unavailable, the queue stays occupied until monitoring recovers. Missing running containers are reported failed only after Docker confirms they no longer exist.
- Logs, snapshots, containers, and artifacts remain until an operator deletes the completed build. Deletion does not clear shared compilation caches. Back up the entire data directory while the service is stopped if preserving build records matters.

## Internet/team deployment

Use the supplied systemd and reverse-proxy templates. The intended request path is:

```text
Browser → Caddy HTTPS on 0.0.0.0:8088 → OAuth2 Proxy GitHub check → Go on 127.0.0.1:8080
```

1. Install the repository and built executable under `/opt/ydfs2`. Create a dedicated `ydfs-build` account with Docker access, ownership of `/var/lib/ydfs-web`, read access to the checkout, and write access to its `.git` directory for the instance lock.
2. Install Caddy and OAuth2 Proxy using their official distribution instructions. Place the OAuth2 Proxy executable at `/usr/local/bin/oauth2-proxy`.
3. Create `/etc/ydfs-web` and copy the three `*.env.example` templates there as `server.env`, `oauth.env`, and `caddy.env`. Restrict them to root (`0600`). Replace every placeholder. Generate the proxy secret with `openssl rand -hex 32` and cookie secret with `openssl rand -base64 32 | tr -- '+/' '-_'`.
4. Register a GitHub OAuth App with homepage `https://YOUR_DOMAIN:8088` and callback `https://YOUR_DOMAIN:8088/oauth2/callback`. Put the app credentials in `oauth.env`. Set the same explicit GitHub username list in `oauth.env` and `server.env`. Use the exact canonical spelling returned by GitHub (including case); OAuth2 Proxy compares usernames case-sensitively.
5. Copy `oauth2-proxy.cfg` to `/etc/ydfs-web/`. Install both supplied service files into `/etc/systemd/system/`. Install the supplied Caddyfile as `/etc/caddy/Caddyfile` and `caddy-environment.conf` as `/etc/systemd/system/caddy.service.d/environment.conf`. Caddy and Go must share the exact proxy secret.
6. Point the domain to this host and open TCP port 8088 for the interface. Caddy’s automatic certificate issuance additionally requires a reachable ACME challenge on port 80 or 443. If only 8088 can be opened, supply an existing certificate with `tls /path/to/fullchain.pem /path/to/privkey.pem` inside the Caddy site block, or configure DNS-based certificate issuance. Run `systemctl daemon-reload`, then enable/start `ydfs-web`, `oauth2-proxy`, and `caddy`.
7. Open `https://YOUR_DOMAIN:8088` and verify an allowed GitHub account can log in; another account and direct unauthenticated requests to port 8080 must fail. Queue a small component build and check streaming through HTTPS before launching an ISO build.

The app accepts identity headers only from loopback with the shared proxy secret and an allowed username. Caddy removes browser-supplied identity headers. Mutating requests also require the configured public Origin and the UI request header. The Go listener has no public unauthenticated mode; do not expose `--dev` through a proxy. All operators can execute the trusted repository build scripts with Docker’s host privileges. Repository/image updates and account administration remain server-admin operations.

Domain registration, OAuth credentials, installing host services, and public activation are deployment tasks; building the application does not perform them.

## API and tests

JSON API: `GET /api/capabilities`, `GET /api/repository`, `POST /api/repository/check`, `POST /api/repository/update`, `GET/POST /api/jobs`, `GET/DELETE /api/jobs/{id}`, `POST /api/jobs/{id}/cancel`, `GET /api/jobs/{id}/events`, `GET /api/jobs/{id}/log`, `GET /api/jobs/{id}/artifacts/{name}`, `GET /api/packages`, `GET /api/packages/{name}`, `GET/PUT /api/profiles`, and `DELETE /api/profiles/{name}`.

Settings shape: `{ "target": "fast-iso", "verbose": true, "kernel": "", "configOverrides": "", "packageList": "", "packageListText": "" }`. Profile shape: `{ "name": "Daily ISO", "settings": { ... } }`. SSE log events contain JSON strings, with byte offsets as event IDs; reconnect with `Last-Event-ID` or `?offset=`. A `done` event contains the completed job. Browser memory is capped to the latest 500,000 characters; downloads retain complete logs.

```sh
make test
# Real Docker lifecycle fixture (small Alpine image; no distro build):
YDFS_DOCKER_TEST=1 go test -race -run TestDockerLifecycle -v .
# UI tests (install Playwright Chromium once):
cd ui
npx playwright install chromium
npm test
```

A real fast-ISO smoke build is separate from routine tests because it downloads substantial prebuilt data and compiles packages. Launch it through the UI, follow the log, and verify the produced ISO boots before distributing it. Native builds, arbitrary shell access, multiple workers, and additional architectures are outside this version.

See [the implementation validation record](VALIDATION.md) for the checks actually run and the limits of the real ISO smoke test.


### Existing Nginx / linuxconsole.ludora.studio

This host already has Nginx and a valid domain certificate managed by Hestia. `deploy/nginx-linuxconsole.conf.template` provides an alternative to Caddy: HTTPS on `0.0.0.0:8088`, using that certificate and the same localhost Go/OAuth2 Proxy services. Render `__YDFS_PROXY_SECRET__` with the shared server secret and install the result as a separate Nginx site (outside Hestia's generated domain files). Validate with `nginx -t` before reloading.

Use `YDFS_ORIGIN=https://linuxconsole.ludora.studio:8088` and OAuth callback `https://linuxconsole.ludora.studio:8088/oauth2/callback`. Set `OAUTH2_PROXY_WHITELIST_DOMAINS=linuxconsole.ludora.studio:8088` to allow redirects back to the nonstandard HTTPS port. The GitHub allowlist and OAuth application credentials must be supplied before activation. This setup needs only TCP 8088 opened for access; certificate renewal remains managed by the existing domain setup.
