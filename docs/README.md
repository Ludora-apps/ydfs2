# YDFS2 / LinuxConsole documentation

This `docs/` folder is the map of the repository: how the ISO gets built, which
script does what, how to customize a build, and how the web build manager
works. It complements, but does not replace, the top-level [README.md](../README.md),
[TIPS.md](../TIPS.md) and [2.12/README.md](../2.12/README.md).

| Document | Covers |
| --- | --- |
| [build-process.md](build-process.md) | The end-to-end build pipeline: `make` targets, Docker images, what each stage produces |
| [scripts-reference.md](scripts-reference.md) | Every script under `2.12/scripts/` and `2.12/Makefile-docker`, grouped by purpose |
| [customization.md](customization.md) | `config.ini`, environment variables, package lists, kernel/toolchain options — how to build a custom distro variant |
| [webui.md](webui.md) | The Go + React web build manager: architecture, tech stack, installation, deployment |

## Quick orientation

The repository builds **LinuxConsole**, a source-based Linux distribution
(Debian-cross-Arch/LFS style: packages are fetched and compiled from source,
not installed from `.deb`/`.rpm`). The active branch is `2.12`, and almost
everything distro-related lives under [`2.12/`](../2.12).

```
ydfs2/
├── 2.12/                  the distro build tree (this is what gets built)
│   ├── Makefile            top-level build entry points (run inside the container)
│   ├── Makefile-docker     host-side targets that wrap `docker compose run`
│   ├── Dockerfile          build image (x86_64), Dockerfile32 (i686)
│   ├── config/             kernel/toolchain/libc configuration fragments
│   ├── scripts/            every build step, as standalone shell scripts
│   ├── packages/           package source lists (list-<name>) per module/arch
│   └── doc/                short build notes (docker.txt, git.txt, qemu.txt)
├── Makefile                 host entry point: `make`, `make full`, `make docker`, ...
├── webui/                   Go + React web UI that drives the same build scripts
├── wubi/                    Windows-based installer (separate subproject)
└── docs/                    you are here
```

Two ways to build:

1. **Command line / Docker Compose** — `make` at the repo root (see
   [build-process.md](build-process.md)).
2. **Web UI** — a browser dashboard that queues and streams the same builds
   (see [webui.md](webui.md)).

Both ultimately call the same scripts in `2.12/scripts/` inside the same
Docker image, so the customization options in
[customization.md](customization.md) apply to either path.
