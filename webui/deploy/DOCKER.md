# Docker deployment

The image compiles the embedded frontend with **Node 20.20.2** and the server
with **Go 1.27.1**, matching the development host. Neither toolchain is needed
on the target server. The runtime includes Git, SSH, GitHub CLI, Docker CLI and
Compose.

Use a **Linux x86_64 server with Docker Engine and Docker Compose**. Distro
builds still target x86_64. The services use host networking to preserve the
server's loopback-only proxy trust and access to the VM's dynamically
published loopback port. Ports 8080 and 4180 must be free; public deployment
also uses 8088 and Caddy's certificate challenge ports 80/443.

From a full clone (including tags), on this branch:

```sh
cd /opt/ydfs2/webui
cp .env.example .env
sudo mkdir -p /var/lib/ydfs-web
chmod 600 .env
# Edit .env: physical absolute checkout/data paths, domain, users and secrets.
docker compose --profile public up -d --build
docker compose logs -f web
```

Create a GitHub OAuth App with homepage `https://YOUR_DOMAIN:8088` and callback
`https://YOUR_DOMAIN:8088/oauth2/callback`. Set its credentials and generated
secrets in `.env` using the commands in the example. Point the domain at the
server and allow TCP 8088 plus 80/443 for certificate issuance. Open
`https://YOUR_DOMAIN:8088`. The `public` profile supplies Caddy and OAuth2 Proxy
using the existing authentication configuration.

If the server already runs the supplied reverse proxy and OAuth2 Proxy,
use `docker compose up -d --build` to start only the application. Match
`YDFS_DOMAIN`, the allowlist and the proxy secret to those services; do not
start a second proxy on occupied ports. The application remains on
`127.0.0.1:8080`. Compose does not change host service configuration.

Both `YDFS_REPO` and `YDFS_DATA` must be existing absolute physical paths without
symlinks or trailing slashes; data must be outside the checkout. They are
mounted at the **same paths** inside the container because the host Docker
daemon resolves build snapshot, cache and artifact bind mounts. Keep these
paths unchanged when restarting a queue. The checkout is writable for
repository operations and the AI editor. The service runs as root with access
to the host Docker socket; allowed operators consequently have the same host
build privileges as the native deployment. Use a dedicated checkout and back
up the data directory with the service stopped. `docker compose down` preserves
build data and certificate volumes (do not add `-v` to preserve certificates).

Browser VM testing is optional and needs a host with working KVM:

```sh
make vm-image
docker compose -f compose.yaml -f compose.kvm.yaml --profile public up -d
```

Without that override the WebUI and builds run normally and VM testing is
reported unavailable. For private Git remotes, configure credentials for the
container explicitly; host SSH keys and GitHub CLI sessions are not mounted
automatically.

To update the application after changing its source, rerun
`docker compose --profile public up -d --build`. Repository switches in the UI
affect build sources; rebuilding the image updates the WebUI.
