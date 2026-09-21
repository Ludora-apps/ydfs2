# Build from webui/: docker build -t ydfs-web .
FROM node:20.20.2-bookworm-slim AS ui
WORKDIR /src/ui
COPY ui/package*.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

FROM golang:1.27.1-bookworm AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/assets/dist ./internal/assets/dist
RUN CGO_ENABLED=0 go build -trimpath -o /out/ydfs-web .

FROM docker:29.5.2-cli AS docker
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git openssh-client gh \
 && rm -rf /var/lib/apt/lists/*
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=server /out/ydfs-web /usr/local/bin/ydfs-web
# Only trust the mounted checkout, not every repository on the filesystem.
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint
ENTRYPOINT ["docker-entrypoint"]
