# Deployment on linuxconsole.ludora.studio

Activated 2026-09-05 at `https://linuxconsole.ludora.studio:8088`.

- Nginx listens on `0.0.0.0:8088`, using the existing Hestia-managed domain certificate.
- Site configuration: `/etc/nginx/conf.d/ydfs-web-8088.conf` (contains the private proxy secret; root-only).
- Go service: `ydfs-web.service`, executable `/usr/local/bin/ydfs-web`, listening on `127.0.0.1:8080`.
- Authentication service: `oauth2-proxy.service`, executable `/usr/local/bin/oauth2-proxy`, listening on `127.0.0.1:4180`.
- Allowed GitHub accounts: `Boyquotes`, `yledoare`.
- Credentials: `/etc/ydfs-web/server.env` and `/etc/ydfs-web/oauth.env`, mode 0600, outside the repository.
- OAuth callback: `https://linuxconsole.ludora.studio:8088/oauth2/callback`.
- Persistent history, artifacts, and cache: `/home/debian/.local/share/ydfs-web`.
- Both application services are enabled at boot. The existing Nginx service remains in use.

Validated: trusted HTTPS response, GitHub authorization redirect with the configured callback, both allowed accounts at the backend authorization boundary, denial of an unlisted account, and rejection of spoofed public identity headers. The interactive GitHub login must be completed by an authorized account owner. Firewall rules were not changed.

The test-machine console (`/api/vm/console`) is a WebSocket. `location /` in `/etc/nginx/conf.d/ydfs-web-8088.conf` must therefore carry `proxy_set_header Upgrade $http_upgrade;` and `proxy_set_header Connection $ydfs_connection_upgrade;` (replacing `proxy_set_header Connection "";`) with the `map $http_upgrade $ydfs_connection_upgrade` block at `http` level, and `proxy_read_timeout`/`proxy_send_timeout` at `7200s` so a still screen is not cut. Build the QEMU image once with `cd webui && make vm-image`; `GET /api/capabilities` reports `vm: false` with a reason until it exists. Back the site file up to `/root` and run `sudo nginx -t` before reloading.

## Signing in and out

The login path is shared between three pieces and all three matter:

- **Only a page navigation may start a login.** `$ydfs_background` in the site
  file turns an unauthenticated API request into a plain `401` instead of a
  redirect to `/oauth2/start`. An open page polls every five seconds; when those
  polls were redirected, each tick began a login of its own, and the first
  callback to complete cleared the shared CSRF cookie out from under the rest —
  the `403` on `/oauth2/callback` ("Cookies were found in OAuth callback, but
  none was a CSRF cookie"). The page turns that `401` into a Sign in button.
- **One CSRF cookie per attempt.** `cookie_csrf_per_request` in
  `oauth2-proxy.cfg` names the cookie after the state, so two logins in flight —
  a second tab, a restored window, a double click — no longer fight over one.
- **A failed callback restarts the login once.** A replayed authorization code
  (a reload or the back button on the callback URL) answers `500`, and a stale
  state answers `403`; both used to leave oauth2-proxy's error page, whose Sign
  In button restarted the login *at that URL* and failed again for ever.
  `@login_retry` retries once, marked by a 120-second cookie, then lands on
  `/signed-out`. `X-Auth-Request-Redirect` is pinned to the site root for the
  same reason — it must never name an `/oauth2/` URL.
- `/signed-out` is the only page served without a session, so signing out does
  not immediately sign the operator back in.

Check the whole path after touching any of it:

```sh
B=https://linuxconsole.ludora.studio:8088
curl -sk -o /dev/null -D- "$B/api/capabilities" -H 'Sec-Fetch-Mode: cors' | head -1   # 401
curl -sk -o /dev/null -D- "$B/" -H 'Sec-Fetch-Mode: navigate' | grep -i location      # /oauth2/start?rd=%2F
curl -sk -o /dev/null -D- "$B/oauth2/sign_out" | grep -i location                     # /signed-out
curl -sk -o /dev/null -D- "$B/oauth2/callback?code=x&state=y" | grep -i location      # one retry
curl -sk -o /dev/null -D- "$B/oauth2/start?rd=%2F" | grep -i 'set-cookie'             # _<nonce>_csrf
```

After rebuilding the application:

```sh
sudo install -m 0755 webui/ydfs-web /usr/local/bin/ydfs-web
sudo systemctl restart ydfs-web
```

After changing the site file, render it from
`webui/deploy/nginx-linuxconsole.conf.template` (substituting the proxy secret),
keep a copy in `/root/ydfs-web-backup/`, and `sudo nginx -t` before reloading.
`sudo install -m 0644 webui/deploy/oauth2-proxy.cfg /etc/ydfs-web/oauth2-proxy.cfg`
then `sudo systemctl restart oauth2-proxy` for the authentication settings.

Service logs: `sudo journalctl -u ydfs-web -u oauth2-proxy`. Validate Nginx before reloading: `sudo nginx -t`.

GitHub OAuth account restrictions are case-sensitive: the canonical API login is `Boyquotes` (capital B). The application backend normalizes case, but OAuth2 Proxy requires the exact GitHub spelling.
