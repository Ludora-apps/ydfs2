# Build process

The distro build has two layers of `make`:

- **Host layer** — the top-level [`Makefile`](../Makefile) and
  [`2.12/Makefile-docker`](../2.12/Makefile-docker). These run on your machine,
  build/use the Docker image, and `docker compose run` the container with the
  right target and environment variables.
- **Container layer** — [`2.12/Makefile`](../2.12/Makefile). This runs
  *inside* the build container and drives the actual compilation by calling
  the scripts in `2.12/scripts/`.

You normally only ever type `make <target>` at the repo root; it takes care of
crossing into the container for you.

## Requirements

- Linux host (x86_64) with Docker Engine + Docker Compose plugin — the README
  strongly recommends the official Docker image over building on bare metal.
- A Debian/Ubuntu host if you build without Docker (`nodocker-debian` target
  installs a long list of native toolchain packages via `apt-get`).
- A writable `$HOME/<branch>` tree for build outputs (`make prepare` creates
  and `chmod 777`s it, since the container runs as an unprivileged user).

## Fast build (recommended default)

```sh
make
```

This is the default target. It:

1. Detects whether Docker is available (`HASDOCKER`) — falls back to native
   `apt-get` build on Debian, or refuses on other systems.
2. Runs `prepare` (creates `$HOME/<branch>/{multilib,x86_64,ydfs,mate,cinnamon,
   linuxconsole,kde,llvm-multilib,opkg}` and `$HOME/iso`, world-writable).
3. Runs `fast-iso`: downloads pre-built core module + kernel artifacts instead
   of compiling them, then builds the "update module" and the ISO on top.

## Full build

```sh
make full
```

Builds everything from source: every package, the kernel, all modules, then
the ISO. This is the `iso-docker` target and can take hours to days.

## Step-by-step / manual control

```sh
make prepare        # fix up local folder permissions for the containers
make docker         # docker build ./2.12 -f 2.12/Dockerfile -t ydfs-2.12
make iso            # full ISO build (same as `make full` minus prepare/docker)
make verbose-iso     # full ISO build with verbose compiler/package output
```

Other host-level targets (`2.12/Makefile-docker` via the root `Makefile`,
all effectively `cd 2.12 && DISTRONAME=linuxconsole make -f Makefile-docker <target>`):

| `make` target | What it does |
| --- | --- |
| `packages` / `opkg` | Build the opkg package repository |
| `kde`, `mate`, `cinnamon`, `kodi` | Build a specific desktop-environment module |
| `virtualbox` | Build the VirtualBox module |
| `multilib` | Build 32-bit compatibility libraries on a 64-bit build |
| `busybox` | Build BusyBox only |
| `linux` | Build the kernel only |
| `initramfs` | Build BusyBox then the initramfs |
| `updates` | Build the "update module" (incremental package updates) |
| `fast-files` | Fetch the pre-built fast-build artifacts only |
| `live-test` | Boot-test the built system inside the container |
| `openxr` | Build the OpenXR-enabled module |
| `gamejam` | Build the "gamejam" package subset |
| `check-strip-dir` | Regenerate `data/dir-to-strip.txt` (files stripped from the final image) |
| `iso-devtools` | Build the developer-tools ISO variant |
| `bash`, `bash-root`, `sh` | Drop into a shell inside the build container (root or unprivileged) |
| `clean` | Clean build artifacts (`clean-docker`) |
| `uninstall` | Remove installed build outputs |
| `buildme` | Resume/retry a build manually after a failure (see [TIPS.md](../TIPS.md)) |

QEMU helper targets (`qemu`, `qemu-efi`, `qemu-usb`, `qemu-bios-diskinstall`,
`qemu-initramfs*`, …) boot the resulting ISO/kernel/initramfs directly with
`qemu-system-x86_64` for quick testing without burning a USB key.

## What a build actually does (container layer)

Described in [`2.12/README.md`](../2.12/README.md) and mirrored by
`2.12/scripts/make_config_ini`:

1. Pick the target architecture (`x86` or `x86_64`; `YDFS_ARCH` env var, or
   autodetected from `uname -m`). This generates `2.12/config.ini`.
2. Every package listed in `packages/list-$ARCH` (plus any extra module lists
   in `MODULES=`/`YDFS_MODULES=`) is downloaded and compiled from source.
3. The kernel is built from `config/kernel-x86_64/` (or `kernel-x86`, `-arm`,
   `-raspi`, …).
4. Kernel modules, initramfs and the final ISO are assembled
   (`make_modules`, `make_initramfs`, `make_iso`).
5. `make test` / `make live-test` boot-check the result.

## Docker images

- `2.12/Dockerfile` — x86_64 build image.
- `2.12/Dockerfile32` — i686 build image.
- `docker-compose.yml` (inside `2.12/`) — the `ydfs2.12` service used by both
  `Makefile-docker` and the web UI (see [webui.md](webui.md)).

Manual (non-`make`) Docker usage, including running a single package build or
a 32-bit image, is documented in [`2.12/doc/docker.txt`](../2.12/doc/docker.txt).

## Output

- ISO: `$HOME/iso/linuxconsole.iso`.
- Write it to a USB key: `dd if=iso/linuxconsole.iso of=/dev/sdX bs=4M status=progress oflag=sync`.
- Build caches / intermediate outputs: `$HOME/<branch>/...` (per-arch/module
  directories created by `make prepare`).

See also: [scripts-reference.md](scripts-reference.md) for what each
individual script does, and [customization.md](customization.md) for tuning
the build (kernel config, toolchain, package selection).
