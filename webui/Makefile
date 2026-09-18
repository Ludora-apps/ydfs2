GO ?= go
VM_IMAGE ?= ydfs-vm:1
.PHONY: build ui test run vm-image
build: ui
	$(GO) build -trimpath -o ydfs-web .
ui:
	cd ui && npm ci && npm run build
test: ui
	$(GO) test -race ./...
run: build
	./ydfs-web --repo .. --dev
# QEMU runner for the "Test in browser" console. Its own build asserts that the
# QEMU inside has a VNC websocket listener, so a broken image cannot be created.
vm-image:
	docker build -t $(VM_IMAGE) vm
