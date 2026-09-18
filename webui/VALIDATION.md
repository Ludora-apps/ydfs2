# Validation — 2026-09-05

The application was built and exercised on this Linux x86_64 host with Docker Engine 29.5.2 and Compose v5.1.4.

- `make build`: passed; React assets are embedded in `ydfs-web`.
- `go vet ./...`: passed.
- `YDFS_DOCKER_TEST=1 go test -race -v ./...`: all 10 top-level tests passed. The real Docker fixture compiled and ran C++, exercised queue serialization, server restart/log replay, cancellation, failure reporting, and artifact downloads.
- `npm test`: all 4 Playwright tests passed, including profile submission, logs/results/downloads, cancellation, failure, and mobile layout. The desktop screenshot was visually inspected.
- Caddy `validate` and OAuth2 Proxy `--config-test`: both deployment templates passed with placeholder test credentials. A real GitHub login and public HTTPS deployment require administrator-provided credentials/domain and have not been performed.

## Real repository smoke run

Using `yledoare/ydfs-2.12:latest` and repository tag `v2.12.1`, a fast-ISO job downloaded/extracted the prebuilt core and multilib files, compiled BusyBox 1.36.1, and reached initramfs generation. It was deliberately cancelled through the API after this smoke check; the container stopped and the queue advanced. **A complete ISO and boot test were not performed.**

A subsequent BusyBox preset reused the populated cache, completed with exit code 0, and produced a downloadable 2,586,784-byte ELF binary. Its downloaded bytes exactly matched the saved artifact:

```text
SHA256 a080066dda450a527de60d4e1eb7b7e72408c8b2e7899ef87ac3b4a5eb5cb305
```

On the implementation host, those two job records, logs, snapshots, artifacts, and shared cache were preserved in `~/.local/share/ydfs-web`. The temporary test server was stopped. Starting the built executable with the README's default command loads this history. These runtime files are outside Git; a new checkout on another host starts with an empty data directory.
