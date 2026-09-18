# Customizing and configuring a build

A build is configured through three layers, applied in this order:

1. **Environment variables** passed to `make`/`docker compose run`, consumed by
   [`2.12/scripts/make_config_ini`](../2.12/scripts/make_config_ini) to
   generate `2.12/config.ini`.
2. **`config.ini`** itself (regenerated fresh on every run — hand edits are
   lost; edit the generator or export env vars instead), which every other
   script `source`s.
3. **Package/module lists and per-package config fragments** under
   `2.12/packages/` and `2.12/config/`.

## Environment variables

Set before `make`, e.g. `YDFS_ARCH=x86 make full`, or inside
`OPTION_BUILD=` when using `2.12/Makefile-docker` directly (see
[TIPS.md](../TIPS.md)):

| Variable | Effect |
| --- | --- |
| `YDFS_ARCH` | Target architecture: `x86`, `x86_64`, `mini2440`, `raspi`, `raspi-qemu`. Autodetected from `uname -m` if unset. |
| `YDFS_CUSTOM_KERNEL` | Override the kernel version string instead of the arch's default. |
| `KERNEL3=SYSTEM` | Use the running host's kernel headers/build instead of building a specific version (see `OPTION_BUILD=KERNEL3=SYSTEM` in `Makefile-docker`). |
| `KERNEL3=latest` | Scrape kernel.org for the latest release and use that version. |
| `DISTRONAME` | Distro name baked into `config.ini` and used for build-output directory names (default `linuxconsole`). |
| `BUILDYDFS=fast` | Fast-build mode: reuse prebuilt squashfs images (core module, drivers, firmware) instead of compiling them; only the update module + ISO are built fresh. |
| `MODULES` / `YDFS_MODULES` | Space-separated list of module/desktop-environment lists to build in addition to the core arch module (e.g. `mate`, `kde`, `cinnamon`, `games`, `music`, `network`, `emulators`, `fps`, `graphics`). Defaults are set in `make_config_ini`. |
| `BUILDMODULES=YES` | Explicitly enable building extra modules. |
| `BUILDOPKG=YES` | Build the opkg package repository as part of the run. |
| `BUILDPKG=<name>` | Build only a single named package (used with `docker run -e BUILDPKG=...`, see [`2.12/doc/docker.txt`](../2.12/doc/docker.txt)). |
| `DIBAB_VERBOSE_BUILD=YES` | Verbose compiler/package build output instead of summarized logging (`make verbose-iso`, `OPTION_BUILD=DIBAB_VERBOSE_BUILD=YES`). |
| `MENUCONFIG=YES` | Drop into interactive `menuconfig` for toolchain/BusyBox/kernel instead of using the stored defconfig. |
| `TOOLCHAIN` | `crosstool-ng`, `buildroot`, or `FriendlyARM` (ARM targets) — selects how the cross-compiler is built. |
| `CROSSTOOL` | crosstool-ng version to use when `TOOLCHAIN=crosstool-ng`. |
| `FORCE_PREFIX=x86` | Force the install prefix directory (`$HOME/prefix`) regardless of detected arch. |
| `BUILDME=OK` | Drop into an interactive shell before `make` actually starts building (useful to inspect/tweak the environment first). |
| `FORCEBUILD=OK` | Force rebuilding every package even if already built/cached. |
| `SEND_BUILD_LOG` | `YES`/`NO` — whether to upload build logs (used by the legacy `docker run` flow). |
| `SEND_OPKG` | `YES`/`NO` — whether to publish built opkg packages. |
| `SLEEPTIME` | Seconds the container idles after a build finishes (for `docker logs -f` workflows), default `3600`. |
| `SKIP_STRIP` | If set, skip stripping symbols from built binaries in `make_module`. |

## `packages/list-*` — choosing what gets built

Each `2.12/packages/list-<name>` file is one package source per line
(tarball URL or `git://…#tag=<ref>`). `#` starts a comment. To skip a range of
packages without deleting them (fast local iteration), wrap them in
`#SKIP` / `#STOP` markers (see [TIPS.md](../TIPS.md)):

```
#SKIP
git://example.com/big-package-to-skip.git
#STOP
```

To add a package to a build: append its source URL to the relevant list
(e.g. `list-x86_64` for the core module, `list-mate` for the Mate desktop,
or create/extend a custom list and reference it via `MODULES=`/`YDFS_MODULES=`).
Local patches live in `2.12/packages/patches/`.

Use `bash 2.12/scripts/check_url` to verify every URL in a list resolves
before kicking off a long build, and
`bash 2.12/scripts/echo-archpkg <pkg>` to check the latest upstream version
Arch Linux packages for reference.

## Per-package build overrides

`2.12/scripts/includes/<pkg>/` holds shell fragments sourced during that
package's build for packages needing special-cased configure/build steps
(e.g. `qt5`, `wine`, `chromium`, `firefox`, `virtualbox`, `nvidia`, `krb5`,
`mame`, `0ad`, `bluez`, `mozjs`, `gmic`). To customize how a specific package
is built, add or edit the fragment under its `includes/<pkg>/` directory
rather than editing `make_packages`/`make_opkg` directly.

`2.12/opkg/<name>/` directories define standalone opkg-format packages (e.g.
`chrome`, `firefox`, `thunderbird`, `fixes`) built via `make_opkg`
(`make chrome`, `make firefox`, …).

## Kernel and toolchain configuration

`2.12/config/` holds the non-generated configuration inputs:

- `kernel-x86_64/`, `kernel-x86/`, `kernel-arm/`, `kernel-raspi*/` — one
  subdirectory per built kernel version, containing the `.config` used as a
  base. To customize kernel options, either edit the relevant version's
  `config` file directly, or set `MENUCONFIG=YES` to get an interactive
  `menuconfig` session during the build (persist your changes back into the
  matching `config/kernel-*/<version>/config` file afterwards, since
  `config.ini` and container state are not preserved between runs).
- `crosstool/`, `crosstool-x86/`, `crosstool-x86_64/`, `crosstool-arm/`,
  `crosstool-i586/`, `crosstool-raspi/` — crosstool-ng configuration per
  target.
- `busybox-x86/`, `busybox-arm/` — BusyBox `.config` per architecture.
- `buildroot/`, `uclibc/`, `uclibc-arm/`, `barebox/`, `nss/` — configuration
  for those components on architectures that use them (mainly ARM/embedded
  targets).
- `Makefile.disable` — flags to disable specific `2.12/Makefile` targets.
- `diff-from-archlinux` — tracked deltas against Arch Linux's own
  configuration choices, for reference when updating a package.

## Building a custom distro variant end to end

A typical "custom spin" workflow:

1. Pick or create a module package list under `2.12/packages/` (copy
   `list-linuxconsole` as a starting point, or set `YDFS_MODULES` to combine
   existing lists).
2. Set `DISTRONAME=yourdistro` so build-output paths and the generated
   `config.ini` don't collide with `linuxconsole`.
3. Adjust kernel/toolchain config under `2.12/config/` if needed, or use
   `MENUCONFIG=YES` for one-off interactive tuning.
4. Build fast first (`BUILDYDFS=fast make`) to validate the module list and
   ISO assembly quickly, then run a full build (`make full`) once satisfied.
5. Boot-test with the `qemu-*` Makefile targets or `make live-test` before
   writing to physical media.

This is exactly what the web UI's build profiles do under the hood — see
[webui.md](webui.md) for driving the same options from a browser instead of
the command line.
