# Scripts reference

All scripts referenced here live under [`2.12/scripts/`](../2.12/scripts)
unless noted otherwise, and are invoked from the `2.12/` build tree (they
`source ./config.ini` and expect the environment set up by
[`make_config_ini`](../2.12/scripts/make_config_ini), see
[customization.md](customization.md)). Most are wired up as
[`2.12/Makefile`](../2.12/Makefile) / [`2.12/Makefile-docker`](../2.12/Makefile-docker)
targets rather than run directly.

## Orchestration (`Makefile-docker` → these scripts)

| Script | Purpose |
| --- | --- |
| `make_config_ini` | Generates `config.ini` (arch, distro name, module list, build mode) from environment variables. Runs at the start of every build. |
| `make_toolchain` | Builds or fetches the cross/native toolchain (binutils/gcc) used to compile everything else. |
| `make_crosstool-ng`, `make_crosstool-ng-new` | Build `crosstool-ng` itself and use it to build a toolchain, as an alternative to `make_toolchain` (see `MENUCONFIG`/`CROSSTOOL` options). |
| `make_busybox_toolchain` | Prepares environment/toolchain specifically for a static BusyBox build. |
| `make_busybox` | Builds BusyBox (used both standalone and as the initramfs shell/utils). |
| `make_linux` | Configures and builds the Linux kernel for `$ARCH`/`$KERNEL3` (or the special `SYSTEM` kernel variant). |
| `make_packages` | Iterates a package/module's `packages/list-*` file, downloading and building each source package. |
| `make_multilib` | Builds 32-bit compatibility libraries (`multilib`) on a 64-bit host, with its own `PATH`/`PKG_CONFIG_PATH` isolation. |
| `make_drivers` | Builds (or reuses a cached squashfs of) the `drivers-$ARCH` module. |
| `make_firmwares` | Builds (or reuses a cached squashfs of) the `firmware-$ARCH` module. |
| `make_modules` | Builds the core `$ARCH` module and other module squashfs images; honors `BUILDYDFS=fast` to reuse prebuilt squashfs instead of compiling. |
| `make_module` | Builds/strips/packages a single named module (e.g. `virtualbox`) into a squashfs. |
| `make_initramfs` | Assembles the initramfs (BusyBox + kernel modules + hooks). |
| `make_iso` | Generates the isolinux/UEFI boot menu and assembles the final ISO image. |
| `make_devs` | Creates device nodes (`mknod`) needed inside a module's root. |
| `make_udev` | Cross-compiles udev for the target `uclibc` environment. |
| `make_test`, `make_live_test` | Boot the built system/module under QEMU (`make_test`) or inside the running container (`make_live_test`) as a smoke test. |
| `make_fast_files` | Downloads the pre-built "fast build" artifacts (core module, kernel) used by `fast-iso`, keyed by `$YDFS`/`$YDFS_TAG`. |
| `check_strip_dir` | Walks built module trees to produce `data/dir-to-strip.txt` (files removed from the final image to save space). |

## Package/opkg helpers

| Script | Purpose |
| --- | --- |
| `make_opkg` | Builds a single `opkg/<name>/` package definition into an installable package. |
| `make_custom_opkg` | Rebuilds one named opkg package, wiping its previous output first. |
| `make_opkg_filelist` | Regenerates the opkg repository index (`Packages`) and optionally publishes it to `/var/www/opkg`. |
| `make_clean_opkg` | Cleans opkg build state (`build-$ARCH`, `src`, `modules`) while preserving a couple of source trees. |
| `make_chrome`, `make_firefox`, `make_thunderbird`, `make_fixes` | Thin wrappers around `make_opkg` for specific browser/runtime packages that need pre-build cleanup. |
| `sort-pkg-by-size` | Lists built packages sorted by installed size (disk-usage triage). |
| `uninstall-package`, `uninstall-package-after`, `uninstall-xorg` | Remove an installed package (or all Xorg-server packages) from a built package tree. |

## Arch Linux packaging bridge

LinuxConsole reuses Arch Linux's `PKGBUILD` recipes/patches as a reference
source for some packages:

| Script | Purpose |
| --- | --- |
| `echo-archpkg <pkg>` | Prints the latest version available in the Arch package tree for `<pkg>` (see [TIPS.md](../TIPS.md)). |
| `get-archpkg <pkg>` | Downloads the Arch `PKGBUILD` for `<pkg>`. |
| `get-archpkg-patches <pkg>` | Downloads the patch files that ship alongside an Arch package. |
| `get-all-archpkg-patches` | Bulk-downloads patches for every Arch package referenced. |
| `build-archpkg <pkg>` | Downloads and builds a package straight from its Arch `PKGBUILD`. |
| `patch-archpkg <pkg>` | Applies the fetched Arch patches to a local source tree. |

## Toolchain / environment

| Script | Purpose |
| --- | --- |
| `first-env` | Snapshots the initial `CFLAGS`/`CXXFLAGS`/`CXX` into `$HOME/ydfs/firstenv` before the build environment is customized. |
| `clear-env` | Dumps and clears build-time environment variables (debug aid for `BUILDME=OK` shells). |
| `build-envars` (doc, not a script) | See [`2.12/build-envars`](../2.12/build-envars) for `FORCE_PREFIX`, `BUILDME`, `FORCEBUILD`. |
| `xvfb-run` | Runs a command under a virtual X server (Xvfb) — used by packages whose build needs a display (e.g. Qt). |
| `install-freepascal.sh`, `install-catalyst.sh`, `install-catalyst13.sh` | Third-party install scripts for Free Pascal and AMD/ATI Catalyst drivers, invoked by the corresponding package builds. |
| `build-llvm-clang` | Downloads and builds LLVM/Clang tools from upstream release tarballs. |
| `build-steam` | Fetches and extracts the Debian Steam installer package. |
| `get_wine_progs` | Pre-installs Windows programs into Wine using `packages/list-linuxconsole-wineprogs`. |
| `install_linux_modules` | Installs built kernel modules into the target module tree (`make modules_install` equivalent). |

## Cleanup

| Script | Purpose |
| --- | --- |
| `make_clean` | Removes `$HOME/$ARCH`, build/src trees, and `~/.cpan` — a broad reset. |
| `make_clean_multilib` | Removes just the multilib build output. |
| `make_distclean` | Runs `distclean` for every package under `build-$ARCH`. |

## Distribution / release

| Script | Purpose |
| --- | --- |
| `install-desktop-menus` | Packages and uploads the desktop menu files to the opkg server. |
| `iso_to_metalink` | Emits a Metalink XML manifest for an ISO release. |
| `upload_logs` | Collects `config.log` files from failed builds for troubleshooting/upload. |
| `print_desktop` | Renders a `.desktop` entry from `$NAME`/`$GENERICNAME`/`$COMMENT`/`$EXEC` env vars. |
| `live-usb` | Detects a plugged-in USB storage device (vendor/product) for live-USB helper scripts. |
| `list-initramfs-modules` | Static list of kernel modules bundled into the initramfs. |

## Misc data files (not scripts)

- `maxmemory`, `archgrouptomodule`, `archtoydfs`, `latest-mate` — small
  lookup/config data files read by the scripts above.
- `check_url` — verifies every URL in a `packages/list-$ARCH` file is
  reachable, without building anything (useful before a big build).
- `build-docbook.sh` — builds the DocBook toolchain used to render
  documentation packages.
- `includes/` — per-package build fragments sourced by `make_opkg`/`make_packages`
  for packages needing special handling (e.g. `qt5`, `wine`, `chromium`,
  `firefox`, `virtualbox`, `nvidia`, `krb5`, `mame`, `0ad`, `bluez`, `gmic`,
  `mozjs`, `scons`, `setuptools`, `os-prober`, `pam`, `git`, `hooks`, `login`,
  `build`, `build-options`, `docker`, `linuxfr.org`, `warsow`, `xbmc`,
  `zyn-fusion`, `fftw`, `games`).
- `persistant-data/` — data preserved across a `make clean` (not wiped by
  the cleanup scripts above).

## Where package sources come from

`2.12/packages/list-<name>` files (e.g. `list-x86_64`, `list-kde`,
`list-mate`, `list-linuxconsole`, `list-firefox`, `list-updates`, …) are plain
text lists of source URLs (tarball links or `git://…#tag=` references), one
package per line, `#`-comments allowed. `make_packages`/`make_opkg` read
these lists to know what to fetch and build; `packages/patches/` holds local
patches applied on top. See [customization.md](customization.md) for how to
add/remove entries and pick which lists are built.
