# About Your Distro From Scratch 2026

This branch is designed to build LinuxConsole 2026 ISO, modules and packages
![logo](/2.12/logos/linuxconsole.png)

> Using Docker for the full build is highly recommended, building whithout official docker image will need your own hacks

# Fast build (makes Iso)

> With this mode, pre-build files for the core module and kernel are downloaded before building "update module" and build ISO

```
make
```

# Building All (makes Iso)

> You will have to wait for hours or days !

```
make full
```

# Building - Step by step

> Activate local folder rights to be used with docker containers
```
make prepare
```

> Build Docker image

```
make docker
```

> Build iso
```
make iso
```

> Verbose build iso
```
make verbose-iso
```

# Documentation
[docs/](/docs/README.md) — build process, scripts reference, custom build
configuration, and the web UI (tech stack + installation).

# Tips
[Tips](/TIPS.md)

# News
[News](/NEWS.md)

# Todo
[TODO](/TODO.md)

# Web build interface

A Go + React build manager provides guided Docker builds, live compilation logs,
profiles, a persistent queue, and authenticated team access.
See [web interface setup and deployment](webui/README.md).

```sh
cd webui
make build
./ydfs-web --repo .. --dev --port 8080
```
