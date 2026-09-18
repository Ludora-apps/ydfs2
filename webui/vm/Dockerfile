# Ephemeral QEMU/KVM runner for the "Test in browser" console (see webui/vm.go).
# No persistent disk and no entrypoint script: QEMU is PID 1, so `docker stop`
# delivers SIGTERM straight to it, and the whole argv comes from the server.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends qemu-system-x86 seabios ipxe-qemu \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /iso && : > /iso/live.iso
# Websocket VNC is an invariant of this image, not a runtime surprise: start a
# headless QEMU and assert the listener exists (15700 == 0x3D54 in /proc/net/tcp).
RUN set -eu; \
    qemu-system-x86_64 -machine q35 -m 64 -S -display none -monitor none \
        -vnc unix:/tmp/probe.sock,websocket=15700 -daemonize; \
    sleep 1; \
    grep -qi ':3D54' /proc/net/tcp || { echo 'no VNC websocket listener'; exit 1; }; \
    rm -f /tmp/probe.sock
ENTRYPOINT []
CMD ["qemu-system-x86_64", "-version"]
