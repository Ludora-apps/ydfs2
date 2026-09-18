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

After rebuilding the application:

```sh
sudo install -m 0755 webui/ydfs-web /usr/local/bin/ydfs-web
sudo systemctl restart ydfs-web
```

Service logs: `sudo journalctl -u ydfs-web -u oauth2-proxy`. Validate Nginx before reloading: `sudo nginx -t`.

GitHub OAuth account restrictions are case-sensitive: the canonical API login is `Boyquotes` (capital B). The application backend normalizes case, but OAuth2 Proxy requires the exact GitHub spelling.
