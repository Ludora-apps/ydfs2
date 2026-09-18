# Pre-installing Flathub applications into the ISO

The build form in the [web UI](webui.md) carries a **Flathub applications** box:
a list of the twenty most popular Flathub applications, fetched live from
Flathub. Whatever the operator ticks is downloaded *during the build* and baked
into the generated ISO, so those applications are present and usable on first
boot with no network connection at all.

This page records what already existed, why the design looks the way it does,
every file involved, and — importantly — which parts are verified and which
still need a boot test.

## What the distro already had

The key finding when this was designed: **LinuxConsole was already a complete
Flatpak host.** Nothing about the Flatpak runtime needed building; only the
*seeding* was missing.

| Piece | Where | Note |
| --- | --- | --- |
| bubblewrap 0.11.1 | [`2.12/packages/list-x86:982`](../2.12/packages/list-x86) | Under a literal `#Flatpak` section at line 981 |
| libostree 2026.1 | [`2.12/packages/list-x86:983`](../2.12/packages/list-x86) | |
| AppStream 1.1.2 | [`2.12/packages/list-x86:988`](../2.12/packages/list-x86) | plus appstream-glib at line 740 |
| flatpak 1.17.6 | [`2.12/packages/list-x86:990`](../2.12/packages/list-x86) | Compiled into the core `x86_64.squashfs` |
| flatpak + libostree rebuilt by `make updates` | [`2.12/packages/list-updates:4-5`](../2.12/packages/list-updates) | **Why baking works on both ISO paths** — see below |
| Persistence loopback | [`init-x86/ydfs/enable/flatpak-disk`](../2.12/init-x86/ydfs/enable/flatpak-disk) | Mounts `/disk/flatpak.loop`, sourced from `rcS:79` |
| Desktop-menu export watcher | [`init-x86/ydfs/fix/flatpak-desktop`](../2.12/init-x86/ydfs/fix/flatpak-desktop) | Copies exported `.desktop`/icons into `/share`, started by `ydfs/start/desktop:25` |
| `XDG_DATA_DIRS` already pointed at Flatpak exports | [`init-x86/etc/profile:4`](../2.12/init-x86/etc/profile) | |
| Manual Flathub setup | [`init-x86/fix/flathub`](../2.12/init-x86/fix/flathub) | `remote-add` + `install` by hand — **never invoked automatically** |

`scripts/make_modules:10` runs `make updates` even in `BUILDYDFS=fast` mode, and
`list-updates` rebuilds flatpak and libostree. That means
`$HOME/x86_64/bin/flatpak` exists inside the build container for **both**
`fast-iso` and `full-iso`, which is what makes build-time downloading possible
at all.

## The decision

Two readings of "pre-installed" were possible:

| Option | What it means | Verdict |
| --- | --- | --- |
| **Seed for first boot** | Ship the app IDs in the initramfs plus a boot script that runs `flatpak install` once the network is up | Not chosen. ISO stays tiny, but needs internet on first boot |
| **Bake into the ISO** | Download during the build, ship the applications on the image | **Chosen.** Truly offline; costs ISO size |

Consequences of choosing to bake:

- Nothing downloads at boot, so there is no re-download-every-boot problem, and
  `/disk/flatpak.loop` is **not** required for the pre-installed applications.
  Persistence remains relevant only for user data and for applications added
  later by the user.
- The catalogue is fetched **live from Flathub and cached server-side**, with a
  built-in list as the cold-start/offline fallback.

### Why a separate squashfs module, not the core rootfs

This is the load-bearing design decision:

1. In `fast-iso` the core `x86_64.squashfs` is **downloaded prebuilt**
   (`scripts/make_modules:5-23` symlinks it and exits) and cannot be modified.
2. `init-x86/ydfs/detect/modules:26` does `cp -fR /ydfs/modules/$ARCH/var /` —
   the core module's `/var` is copied into the root filesystem on **every
   boot**. The root is a tmpfs sized at 75% of RAM
   (`init-x86/init-newroot:11`), so putting multi-gigabyte Flatpak data in the
   core `/var` would be fatal.

A separate module avoids both, stays compressed and read-only on the ISO, costs
no RAM, and rides machinery that is already fully generic: `scripts/make_module`
builds an arbitrary `$HOME/<name>` into `<name>-$ARCH.squashfs`,
`scripts/make_iso:324-333` ships every name listed in `MODULES=`, and
`init-x86/ydfs/start/mountopt:4-36` auto-mounts and overlays anything it finds —
with no changes to any of them.

## How it works

```
web UI form  ──►  snapshot: 2.12/data/flathub-apps      (one app ID per line)
                         │
   scripts/make_config_ini ──┤ list non-empty ⇒ append flatpak-${ARCH} to MODULES=
                         │
   make flatpak  ──►  scripts/make_flatpak         (flatpak install → $HOME/flatpak)
                 ──►  scripts/make_module flatpak  (→ $LCMBUILD/flatpak-x86_64.squashfs)
                         │
   scripts/make_iso ─────┤ symlinks every $MODULES entry into iso/modules
                         │
   boot: ydfs/start/mountopt          mounts + overlays it   (unchanged)
         ydfs/enable/flatpak-preinstalled   points Flatpak at it
         ydfs/fix/flatpak-desktop     exports menus          (unchanged)
```

An empty list short-circuits every step: `FLATPAKMODULE` stays empty, `MODULES=`
is unchanged, no `flatpak` target runs, and the ISO is built exactly as before.

## Files

### Distro build (`2.12/`)

| File | Role |
| --- | --- |
| [`data/flathub-apps`](../2.12/data/flathub-apps) | **New.** One Flathub application ID per line; `#` comments and blank lines ignored. Ships commented-out, so default builds are unchanged. Hand-editable for command-line builds. |
| [`scripts/make_flatpak`](../2.12/scripts/make_flatpak) | **New.** Re-validates every ID, adds the Flathub remote, installs into a staging installation, and prunes applications dropped from the list. |
| [`Makefile`](../2.12/Makefile) | **Modified.** `FLATHUB_APPS`/`FLATPAKMODULE` variables, a `flatpak` target, and `${FLATPAKMODULE}` added to the `${ISOPATH}` prerequisites. |
| [`scripts/make_config_ini`](../2.12/scripts/make_config_ini) | **Modified.** Appends `flatpak-${ARCH}` to the generated `MODULES=` line when — and only when — the list is non-empty. |
| [`init-x86/ydfs/enable/flatpak-preinstalled`](../2.12/init-x86/ydfs/enable/flatpak-preinstalled) | **New.** Boot-time wiring: points Flatpak at the mounted module. |
| [`init-x86/etc/init.d/rcS`](../2.12/init-x86/etc/init.d/rcS) | **Modified.** One line at `rcS:78`, sourcing the above just before `flatpak-disk`. |

Anything placed under `2.12/init-x86/` ships in the initramfs verbatim —
`scripts/make_initramfs:117` is a plain `cp -fR init-$ARCH/* $INITRAMFS` — so
the new boot script needed no build-system change to be included.

### Web UI (`webui/`)

| File | Role |
| --- | --- |
| [`flathub.go`](../webui/flathub.go) | **New.** Catalogue type, built-in fallback list, cache, Flathub fetch, the two HTTP handlers, and `allowedFlatpak` (the server-side allowlist). |
| [`model.go`](../webui/model.go) | `Settings.Flatpaks []string`, `flatpakIDPattern`, `maxFlatpakApps`, `validateFlatpaks`, and `Settings.normalize`. |
| [`runner.go`](../webui/runner.go) | `capabilities` serves the catalogue; `submit` enforces the allowlist and writes `data/flathub-apps` into the job's snapshot. |
| [`main.go`](../webui/main.go) | Three `App` fields and the two routes. |
| [`ui/src/main.tsx`](../webui/ui/src/main.tsx) | The picker, the refresh action, and the `blankSettings` default used when loading older profiles. |
| [`ui/src/style.css`](../webui/ui/src/style.css) | `.apps-field` / `.apps` / `.apps-actions`, collapsing to one column at ≤740px. |

## Using it

### From the web UI

The box appears for **Fast ISO** and **Full ISO** targets only, and is hidden
for component builds. Nothing is ticked by default. "Refresh from Flathub"
re-fetches the popular collection; the footer shows the selection count and when
the catalogue was last updated.

### From the command line

Edit [`2.12/data/flathub-apps`](../2.12/data/flathub-apps), one application ID
per line, then build as usual:

```sh
# 2.12/data/flathub-apps
org.videolan.VLC
com.github.tchx84.Flatseal
```

```sh
make            # fast ISO, now carrying the two applications above
make flatpak     # build just the flatpak module
```

The web UI never writes to the checkout's own copy of this file: each submission
rewrites it inside that job's private snapshot only, exactly like the
`packageList` override.

### API

| Endpoint | Behaviour |
| --- | --- |
| `GET /api/flathub` | The offered catalogue, **no network call** — the last refresh if there is one, otherwise the built-in list |
| `POST /api/flathub/refresh` | Re-fetches Flathub's popular collection into `<data>/flathub.json`; on failure keeps the previous catalogue and reports the reason |

This mirrors the split already used for the repository box in
[`webui/repo.go`](../webui/repo.go): a cheap no-network `GET`, and an explicit
`POST` that reaches the network.

Settings shape gains `"flatpaks": []`. Old jobs and profiles stored before this
existed simply lack the key; `Settings.normalize` turns the resulting `nil` into
`[]` on every read path, so the API shape stays stable.

## Validation

Application IDs become `flatpak` command arguments and lines of a file the build
reads, so they are validated at three independent layers:

1. **`Settings.validate`** (`webui/model.go`) — ISO targets only, at most
   `maxFlatpakApps` (40), no duplicates, and every ID must match
   `^[A-Za-z][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+){1,8}$`. No spaces, semicolons,
   backticks, `$`, slashes or newlines survive this.
2. **`allowedFlatpak`** (`webui/flathub.go`, enforced in `submit`) — the ID must
   be in the cached catalogue *or* the built-in list. The browser's list is
   never authoritative. The union is deliberate: a refresh can add applications
   but can never lock out an already-saved profile.
3. **`scripts/make_flatpak`** — re-validates with the same pattern, because
   `data/flathub-apps` is hand-editable for command-line builds and must not
   trust whatever the web UI already checked.

Catalogue membership is checked in `submit` rather than inside `validate()`,
matching the existing treatment of `packageList`: a saved profile is allowed to
outlive a catalogue refresh, so profile saving does not enforce membership.

`make_flatpak` installs with `--user` and `FLATPAK_USER_DIR` pointed at the
staging directory, which keeps the whole operation inside `$HOME/flatpak` and
avoids polkit and `flatpak-system-helper` — neither exists in the build
container. `$HOME` is a cache shared by every build, so the script also
uninstalls applications no longer in the list, then drops unused runtimes;
without that, a previous job's selection would leak into the next module.

`SKIP_STRIP=OK` is passed to `make_module` deliberately: an OSTree checkout is a
web of hardlinks into `repo/objects`, and stripping binaries in place would
corrupt it.

## At boot

`ydfs/start/mountopt` (from `rcS:44`) has already mounted the module at
`/ydfs/modules/flatpak` and overlaid it read-write onto `$HOME_DIBAB/flatpak` by
the time `ydfs/enable/flatpak-preinstalled` runs at `rcS:78`. The script only
has to point Flatpak at it, and takes one of two branches:

| Condition | Behaviour |
| --- | --- |
| No `/disk/flatpak.loop` (common case) | Symlinks `$HOME_DIBAB/$ARCH/var/lib/flatpak` at the overlaid module, making it *the* system installation — which is exactly the path `etc/profile` and `fix/flatpak-desktop` already expect. The overlay upper keeps it writable, so users can still install more. |
| `/disk/flatpak.loop` present | Leaves `flatpak-disk` in charge of `var/lib/flatpak` and registers the baked tree as a second, read-only installation via `/etc/flatpak/installations.d/preinstalled.conf`, appending its `exports/share` to `XDG_DATA_DIRS` through `/etc/env.dyn`. |

Two constraints on that script, both easy to get wrong:

- It is **sourced**, not executed, so it must never call `exit` — that would end
  the boot. It is written as a single `if` block for this reason.
- `$HOME` is not usable that early (busybox init leaves it at `/`), so it uses
  `$HOME_DIBAB` from `/etc/profile` and `$ARCH` from `ydfs/detect/media`. This
  is the same reason `flatpak-disk` hardcodes its paths.

## ISO size

This is the real cost of baking. Twenty popular applications plus the shared
`org.freedesktop.Platform` / `org.gnome.Platform` / `org.kde.Platform` runtimes
is roughly **6–12 GB** before squashfs compression. OSTree hardlinks and
squashfs compression reclaim a good part of that, but a fully-ticked ISO is
past DVD size and USB-only.

Mitigations: nothing is selected by default, the form carries a size warning,
and `make_flatpak` logs `du -sh` of the staging tree while `mksquashfs` reports
the compressed size. Flathub's API exposes **no** size field, so the UI cannot
estimate the growth before the build runs.

## Testing

| Test | Covers |
| --- | --- |
| `TestFlathubValidation` | Target gating, the cap, duplicates, and injection shapes (`;`, `$( )`, backticks, newline, path traversal, bare names) |
| `TestFlathubCatalogue` | Cold-start fallback, a refresh replacing the catalogue, bogus IDs dropped from upstream data, a failed refresh keeping the previous catalogue, and reload from disk after restart |
| `TestFlathubSubmission` | 400 for an off-catalogue ID; 202 writes the snapshot file; the shared checkout stays untouched; an empty selection rewrites nothing |
| `TestFlathubModuleEntersConfiguration` | `MODULES=` gains `flatpak-${ARCH}` only for a non-empty list, under both `make include` and `bash .` sourcing |
| `TestMakeFlatpakRejectsUnsafeApplicationIDs` | The script's own re-validation, and that an empty list is a clean no-op |
| `builds.spec.ts` "Flathub applications are selected into the build" | The picker renders, selection count updates, the box hides for non-ISO targets, and the selection reaches the job |

```sh
cd webui
make build && go vet ./... && go test -race ./...
cd ui && npm test
```

> `MODULES=` is written to `config.ini` *before* `ARCH=`, so a bare
> `. ./config.ini` leaves `${ARCH}` empty. The build scripts never see that,
> because `2.12/Makefile:12` exports `ARCH` before running them — any test that
> sources `config.ini` directly must export `ARCH` first to be faithful.

## What is verified, and what is not

Verified end to end against a running server and a real job snapshot:

- Cold start serves 20 built-in applications with no network call and no cache file.
- A live `POST /api/flathub/refresh` fetched the real Flathub popular collection
  and persisted it to `<data>/flathub.json`.
- An application present only in the refreshed catalogue (not in the built-in
  list) became submittable — the allowlist grows with a refresh.
- All rejection paths return clear 400s: off-catalogue, `;`-injection,
  newline-injection, duplicates, wrong target.
- A valid submission wrote exactly the selected IDs to
  `<data>/jobs/<id>/source/data/flathub-apps`, leaving the checkout untouched.
- Running the real `make_config_ini` *inside that snapshot* produced
  `MODULES="... mate-${ARCH} flatpak-${ARCH}"`, and `make_flatpak` parsed and
  validated the IDs, stopping only at the missing `flatpak` binary (which the
  real container has).
- The picker renders correctly in both light and dark themes and collapses to
  one column on mobile without horizontal overflow.

**Not verified: booting an ISO.** No test in this repository boots the produced
image. The runtime half — whether Flatpak actually finds and launches the baked
applications — is unproven and should be treated as such:

1. Write an ISO built with two or three *small* applications to a USB key.
2. Boot it **with the network disconnected**.
3. Check the applications appear in the desktop menu (via `fix/flatpak-desktop`),
   that `flatpak list` shows them, and that they launch.
4. Repeat on a machine with a `/disk/flatpak.loop` present, to exercise the
   extra-installation branch and confirm `flatpak-disk` still wins for user data.

Expect iteration on step 4. Flatpak's multi-installation config, `XDG_DATA_DIRS`
propagation and the menu-export watcher all have to line up in a distro with no
systemd and no `flatpak-system-helper`; the plain-symlink branch is the
low-risk path, the extra-installation branch is the one most likely to need a
second pass.

## Known risks

- **Runtime wiring** — as above, unproven until a boot test.
- **ISO size** — a fully-ticked selection plausibly exceeds 10 GB.
- **Build time and bandwidth** — downloading applications and runtimes adds a
  large network-bound step to an already long build.
- **`flatpak install` inside the build container** is exercised only up to the
  point of invoking the binary. If `--user` plus `FLATPAK_USER_DIR` turns out to
  need a session bus or a bwrap capability the container lacks, the fallback is
  an `ostree`-level pull plus `flatpak create-usb`-style sideloading, which
  needs no session bus.
- **Flathub API drift** — the cache plus the built-in fallback pin the shape, so
  an upstream schema change degrades to the fallback rather than emptying the
  form.

## Possible future work

- **Wire up `ydfs/install/flatpak-disk`.** It already exists
  (`fallocate -l 20G /disk/flatpak.loop` + `mke2fs`) but nothing calls it;
  [`TODO.md:3`](../TODO.md) tracks this. It would give users a persistent store
  for applications they add themselves.
- **Sideload instead of a full installation.** Shipping the module as an OSTree
  sideload repo (`/run/flatpak/sideload-repos`) rather than a complete
  installation would let baked applications update normally from Flathub while
  still working offline.
- **Per-application size hints.** Flathub's API has no size field today; sizes
  would have to come from the repo summary or a build-time measurement fed back
  into the form.
