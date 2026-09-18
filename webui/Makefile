GO ?= go
.PHONY: build ui test run
build: ui
	$(GO) build -trimpath -o ydfs-web .
ui:
	cd ui && npm ci && npm run build
test: ui
	$(GO) test -race ./...
run: build
	./ydfs-web --repo .. --dev
