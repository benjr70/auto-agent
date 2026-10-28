# Removing only the Daemon's share of build and package caches

Ticket: [#206](https://github.com/benjr70/auto-agent/issues/206) (part of wayfinder map
[#204](https://github.com/benjr70/auto-agent/issues/204), "Map: auto-agent cleans up its Footprint on the Host").
Researched on 2026-10-28 against the source on `master`/`main` of each project, plus read-only
inspection of this Host (Docker 29.4.3, containerd image store, compose v5.1.3, builders
`multiarch` (docker-container, BuildKit v0.30.0) and `default` (docker driver, BuildKit v0.29.0)).

Sources:
- Docker docs, build garbage collection: https://docs.docker.com/build/cache/garbage-collection/
  (raw: `docker/docs` `content/manuals/build/cache/garbage-collection.md`)
- BuildKit config reference: https://github.com/moby/buildkit/blob/master/docs/buildkitd.toml.md
- BuildKit source: `cmd/buildkitd/config/gcpolicy.go`, `gcpolicy_unix.go`, `control/control.go`,
  `cache/manager.go` (`adaptUsageInfo`) in https://github.com/moby/buildkit
- Moby source: `daemon/internal/builder-next/worker/gc.go`, `daemon/internal/builder-next/builder.go`,
  `daemon/internal/builder-next/controller.go`, `daemon/containerd/soft_delete.go` in https://github.com/moby/moby
- buildx reference: https://docs.docker.com/reference/cli/docker/buildx/prune/,
  https://docs.docker.com/reference/cli/docker/buildx/create/, https://docs.docker.com/reference/cli/docker/buildx/rm/,
  https://docs.docker.com/build/building/variables/#buildx_builder,
  https://docs.docker.com/build/builders/drivers/docker-container/
- Compose: https://docs.docker.com/reference/cli/docker/compose/down/; source `pkg/compose/image_pruner.go`,
  `pkg/compose/build_bake.go`, `pkg/compose/down.go` (main and tag v5.1.3) in https://github.com/docker/compose
- npm: https://docs.npmjs.com/cli/configuring-npm/folders#cache, https://docs.npmjs.com/cli/commands/npm-cache,
  https://docs.npmjs.com/cli/using-npm/config#environment-variables, `workspaces/config/lib/definitions/definitions.js`
- Playwright: https://playwright.dev/docs/browsers#managing-browser-binaries; source
  `packages/playwright-core/src/server/registry/index.ts`
- Electron: https://www.electronjs.org/docs/latest/tutorial/installation#cache
- Smart Smoker: `e2e/docker/stack.Dockerfile`, `scripts/stack-runner/derive-compose.ts`,
  `scripts/stack-runner/stack-runner.ts` in benjr70/Smart-Smoker-V2

## TL;DR

- **BuildKit cannot attribute cache to a build, stack or label.** Cache records carry no labels.
  The prune filters are `id`, `parents`, `description`, `type`, `inuse`, `shared`, `private` and `until`.
  Moby accepts a `label` filter but never applies it (`// TODO(tiborvass): handle label`). Records are
  content-addressed, so two PRs' builds share their common stages, and no one stack "owns" those records.
  `docker builder prune` on a shared builder cannot remove only one stack's share.
- **The way to get "only the Daemon's share" is to give the Daemon its own builder.** Create a
  dedicated `docker-container` builder per Daemon (`docker buildx create --name auto-agent-<owner>.<name>
  --driver docker-container --buildkitd-config <tight GC toml>`) and export `BUILDX_BUILDER=<that name>`
  to the Environment provider. Compose's bake path honours it. Bound the builder by its own GC, use
  `docker buildx prune --builder <name> …` for an on-demand trim, and `docker buildx rm <name>` (state
  volume included) on uninstall. The cost is that the image is stored twice, once in the builder's
  volume and once in the engine store, because compose `--load`s the result.
- **GC runs only after a build finishes, never during one.** It runs 1 s after a Solve ends,
  throttled to once a minute, and once at builder start. There is no timer. The ~4.4–5.2 GB left after a
  cold build is by design: GC never goes below `reservedSpace` (here 5 GB = 4.657 GiB, 10% of the 48 GB
  disk). With free space (6.3 GB) already under `minFreeSpace` (9.313 GiB), every sweep trims back to
  that floor.
- **`compose down -v --remove-orphans --rmi local`** removes the project's containers, its networks,
  its named and anonymous volumes, and the images compose tagged `<project>-<service>`. That covers
  Smart Smoker, because `derive-compose.ts` drops the `image:` field. It never touches build cache. In
  compose v5.1.3 it also leaves the project's **dangling** images. On the containerd store a rebuild
  that retags `<project>-<service>` keeps the old image as `moby-dangling@sha256:…` with its labels,
  `com.docker.compose.project` included. `docker image prune -f --filter
  label=com.docker.compose.project=<project>` removes exactly those images.
- **Package caches isolate cleanly by environment variable.** Set `npm_config_cache`,
  `PLAYWRIGHT_BROWSERS_PATH` and `electron_config_cache` (or `XDG_CACHE_HOME`, which covers Playwright,
  Electron and other env-paths users) to directories under the State dir. Removing them is then
  `rm -rf`. Scope them **per State dir, not per Fire**. npm never prunes its cache, so a per-Fire
  cache would mean a full cold download every Fire, which on this Host is ~2.1 GB of npm tarballs, ~650 MB
  of Playwright browsers and ~200 MB of Electron zips. A per-State-dir cache pays that once per Target
  Project.

## 1. BuildKit GC: defaults, timing, and the two drivers

### Defaults (source, not docs)

BuildKit's built-in policy (`cmd/buildkitd/config/gcpolicy.go` `DefaultGCPolicy`) is four ordered rules:

1. `type==source.local,type==exec.cachemount,type==source.git.checkout`, unused for 48h, when that
   class exceeds 512 MB. This covers build contexts, git checkouts and `RUN --mount=type=cache`, which
   is where Smart Smoker's in-build `/root/.npm` lives.
2. Anything unused for 60 days (`keepDuration 1440h`), within reserved/max/min-free.
3. Unshared cache, down to the cap.
4. `all = true`: any cache, internal and frontend records included, down to the cap.

With no thresholds configured, `DetectDefaultGCCap` (`gcpolicy_unix.go`) sets:
- `reservedSpace` = 10% of the disk, capped at 10 GB.
- `maxUsedSpace` = **80%** of the disk, capped at 100 GB. The Docker docs table says 60%, but the
  source constant `DiskSpaceMaxPercentage = 80`, and this Host's 36.32 GiB matches 80%.
- `minFreeSpace` = 20% of the disk.

Percentages round up to whole GB: `(total*pct/100 / GiB + 1) * 1e9`.

The docker driver (Moby's embedded BuildKit, `daemon/internal/builder-next/worker/gc.go`) uses the
same four rules and the same 10/80/20 percentages, with two differences. There are no 10 GB/100 GB caps.
Rule 1's limit is `reservedSpace × e·π·φ%` (≈13.8%), with a 512 MB floor. It is configured in
`daemon.json` under `builder.gc` (`defaultReservedSpace`/`defaultMaxUsedSpace`/`defaultMinFreeSpace`, or
a `policy` list), not in `buildkitd.toml`.

This Host (48 GB root disk) checks out exactly:

| | default (docker driver) | multiarch (docker-container) |
|---|---|---|
| rule 1 max used | 658.9 MiB (13.8% of 5e9) | 488.3 MiB (512 MB) |
| reservedSpace | 4.657 GiB (5e9 = 10%) | 4.657 GiB |
| maxUsedSpace | 36.32 GiB (39e9 = 80%) | 36.32 GiB |
| minFreeSpace | 9.313 GiB (10e9 = 20%) | 9.313 GiB |
| keepDuration (rule 2) | 1440h | 1440h |

The docs say "`reservedSpace` has the highest priority in defining the lower limit… the minimum cache
size would never be brought below `reservedSpace`." `minFreeSpace` makes GC delete least-recently-used
records until free space reaches the target, still floored at `reservedSpace`. The Host has 6.3 GB free,
below the 9.313 GiB `minFreeSpace`, so every sweep cuts the cache back to ~5 GB. That is the residue the
ticket observed, and no prune is needed to explain it.

### When GC runs

From `control/control.go`:
- `c.throttledGC = throttle.After(time.Minute, c.gc)` sets a throttle of one run per minute.
- `NewController` schedules one GC 1 s after the builder starts.
- `Solve` defers `time.AfterFunc(time.Second, c.throttledGC)`, so GC is scheduled **after each build
  request ends**.

There is no ticker, so GC never runs during a build. Records in use by a running build
(`inuse`) are never pruned. If nothing builds, nothing is collected. The docs' "GC runs periodically"
really means "after builds".

### docker vs docker-container driver

- **Where the cache lives.** The docker driver's cache sits in the engine's own store. With the
  containerd snapshotter its snapshots are shared with image layers: a record is `shared` while an
  image references it and becomes `private` once the image is gone. The docker-container driver keeps
  everything in a dedicated volume, `buildx_buildkit_<builder>0_state`. The docker-container driver docs
  call this "Cache persistence".
- **Images.** The docker driver writes images straight into the engine store. The docker-container
  driver must export and load them (compose's bake path loads results "into the local engine",
  `build_bake.go`), so each image exists twice.
- **Configuration.** The docker driver is configured in `daemon.json` `builder.gc`, which needs a daemon
  restart and is Host-wide. The docker-container driver is configured in `buildkitd.toml` given at
  `docker buildx create --buildkitd-config`, per builder.
- **Count.** There is exactly one docker-driver builder per engine and it is shared with the operator.
  You can create as many docker-container builders as you like.

**Host observation (read-only).** `docker buildx du --builder multiarch` reports 0 B of cache, but the
`buildx_buildkit_multiarch0_state` volume is 4.04 GB, and 3.7 GB of that is
`/var/lib/buildkit/runc-overlayfs/metadata_v2.db`. Snapshots are only 232 KB. The bolt metadata DB does
not shrink when records are pruned. BuildKit `master` documents an opt-in `[compaction]` section,
"independent of cache GC", `enabled = false` by default. So `prune` does not get that space back.
Removing the builder's state volume (`docker buildx rm` without `--keep-state`) does.

**Which builder the Daemon uses today.** Compose always builds through `buildx bake` when the buildx plugin
is present (`buildWithBake`), and bake uses the **current** buildx builder unless `--builder` or
`BUILDX_BUILDER` is given. `~/.docker/buildx/current` here is `multiarch`. `buildx history ls` shows
Smart Smoker `stack.Dockerfile` builds on **both** `multiarch` (11 h ago) and `default` (10 h ago). The
Daemon's cache therefore lands in whatever builder happens to be current, mixed with the operator's own.

## 2. Selective pruning

`docker buildx prune` / `docker builder prune` filters (buildx prune reference; BuildKit
`cache/manager.go` `adaptUsageInfo`; Moby `builder.go` `cacheFields`):

- `until=<dur>`: keep records used within the duration.
- `id`, `parents`: specific record IDs and their parents.
- `description`: substring match on the record description. Descriptions are the step text, such as
  `[deps 3/3] RUN --mount=type=cache… npm ci`, and contain nothing per-stack.
- `type`: `internal|frontend|source.local|source.git.checkout|exec.cachemount|regular`.
- `inuse`, `shared`, `private`, and `mutable`/`immutable` (buildx only; Moby's API does not expose them).
- Plus `--reserved-space`, `--max-used-space`, `--min-free-space` and `--all`.

`label` and `label!` are accepted by Moby's validator and ignored (`// TODO(tiborvass): handle label`).
Nothing that compose, a build arg, an image label or the project name sets ends up on a cache record.
Even if it did, records are deduplicated by content: PR 12's and PR 13's `deps` stage are the same
record when the lockfile matches. Per-stack ownership of shared build cache is not well-defined.

What does select cleanly:
- **Per builder.** `--builder <name>` scopes `prune` and `du` to one BuildKit instance. A Daemon-owned
  docker-container builder holds only the Daemon's records. Options:
  - Bound it continuously with a `buildkitd.toml` such as `reservedSpace = "2GB"`,
    `maxUsedSpace = "8GB"`, `minFreeSpace = "15%"`, and a `[history] maxEntries` lower than 50.
  - Trim on demand with `docker buildx prune --builder <name> -f --max-used-space 4gb`.
  - Empty it with `--all`.
  - Erase it, metadata DB included, with `docker buildx rm <name>`.
- **Per time.** `--filter until=<dur>` on a shared builder removes everyone's stale records, not only the
  Daemon's. It is not a share boundary.

## 3. `compose down -v --remove-orphans --rmi local` on the containerd store

Per the compose down reference and `image_pruner.go`:
- **Removes:**
  - Service containers, plus orphans with `--remove-orphans`.
  - Retained `pre_start` hook containers.
  - Project and default networks.
  - With `-v`, named volumes declared in `volumes:` and anonymous volumes attached to the containers.
  - With `--rmi local`, images labelled `com.docker.compose.project=<project>`, not dangling, and still
    tagged, for services without a custom `image:`. It also removes unlabelled images matching the
    implicit `<project>-<service>` name. Smart Smoker's `derive-compose.ts` deletes `image:` from every
    built service precisely so this reclaims `smoker-pr-<N>-<service>`.
- **Leaves:**
  - **All build cache.** `down` never talks to BuildKit.
  - Pulled base/registry images such as `mongo:7.0` (only `--rmi all` takes those).
  - External networks and volumes.
  - **Dangling project images.** `ImagesToPrune` skips images with no `RepoTags`. On the containerd store,
    tagging a new build over an existing name calls `softImageDelete`, which keeps the old image as
    `moby-dangling@<digest>` and copies its labels (`daemon/containerd/soft_delete.go`). A PR rebuilt
    twice before `down` therefore leaves one dangling image per service, labelled with the project.
    Compose `main` has added `removeDanglingImages` to down; v5.1.3 (installed here) has not. Until then,
    after `down` run:
    `docker image prune -f --filter label=com.docker.compose.project=smoker-pr-<N>`.
    Without `-a`, `image prune` only takes dangling images, and the label filter limits it to that one stack.
- No "intermediate images": BuildKit does not create the per-step images the legacy builder did.
  Intermediate state is build cache, covered by sections 1 and 2.
- With the docker-container driver, the loaded image is the engine's copy. Removing it frees the engine
  side, and containerd GCs unreferenced content and snapshots. The builder's copy stays in its volume
  until that builder's GC, prune or rm.

## 4. npm, Playwright and Electron caches

- **npm.** The cache defaults to `~/.npm` on POSIX and does not follow XDG (folders docs;
  `definitions.js`). It is set by the `cache` config. Any `npm_config_<key>` env var sets config
  (config docs), so `npm_config_cache=$STATE/cache/npm` moves `_cacache`, `_npx` and `_tuf` together.
  The npm-cache docs are explicit: "npm will not remove data by itself: the cache will grow as new
  packages are installed". All data is integrity-verified on insert and extract, so clearing it is only
  ever about disk space, and dropping the directory is safe. Note that npm sets its own lowercase
  `npm_config_*` inside `scripts`, which Node prefers. This only matters for nested npm calls from
  lifecycle scripts, and they inherit the same value.
- **Playwright.** Browsers go to `~/.cache/ms-playwright` on Linux (browsers docs). The registry computes
  this as `process.env.XDG_CACHE_HOME || ~/.cache` (`registry/index.ts`), overridable by
  `PLAYWRIGHT_BROWSERS_PATH`. `PLAYWRIGHT_BROWSERS_PATH=0` puts them inside `node_modules` (a "hermetic
  install"). Playwright itself garbage-collects browser versions that no installed Playwright still
  needs ("Stale browser removal").
- **Electron.** `@electron/get` caches to `$XDG_CACHE_HOME` or `~/.cache/electron/` on Linux and is
  overridable by `electron_config_cache` (installation docs, "Cache").
- **In-image npm.** Smart Smoker's `stack.Dockerfile` runs `npm ci` with
  `RUN --mount=type=cache,target=/root/.npm`. That cache is a BuildKit `exec.cachemount` record, not the
  host's npm cache. It belongs to whichever builder ran the build and is aged out by GC rule 1 (48h,
  over 512 MB or 659 MiB). `npm_config_cache` does not reach it. A Daemon-owned builder does.

**Isolation verdict.** Pointing these variables into the State dir isolates the Daemon's package downloads
from the operator's `~/.npm` and `~/.cache`, and cleanup becomes deleting a directory the Daemon owns.
Two cautions apply:
1. Prefer the specific variables (`npm_config_cache`, `PLAYWRIGHT_BROWSERS_PATH`,
   `electron_config_cache`) over a blanket `XDG_CACHE_HOME`. `XDG_CACHE_HOME` also redirects every other
   env-paths/XDG tool in the Fire, the agent CLI's own cache included. If `XDG_CACHE_HOME` is used, set it
   only on the Target Project's commands (install/test/provider), not on the whole Fire.
2. Scope the cache to the State dir, one per Target Project, so it survives across Fires.

**Cost.** A cold cache means a full download of the dependency tree. As a size proxy on this Host:
`~/.npm` is 2.1 GB, `~/.cache/ms-playwright` 646 MB and `~/.cache/electron` 203 MB. Reinstall time was
not measured, because that would require running installs on the Host, which was out of scope. Time
scales with registry bandwidth for those bytes plus native rebuilds (serialport, usb). A per-Fire cache
pays this every Fire. A per-State-dir cache pays it once, and later installs read verified tarballs from the warm cache. A one-off timed
`npm ci` with an empty versus a warm `npm_config_cache` in a scratch clone would put a number on it if
the map needs one.

## Open points for the map

- Should the Daemon own a builder (clean share, ~2× image disk, its own GC knobs) or keep using the
  shared one and only bound it (no per-Daemon share possible)? This research supports a Daemon-owned
  docker-container builder named after the Daemon unit, created at install and removed at uninstall.
- Uninstall should `docker buildx rm` that builder without `--keep-state`. Pruning alone leaves the
  bolt metadata DB, which is 3.7 GB on `multiarch` here.
- Teardown should add `docker image prune -f --filter label=com.docker.compose.project=<project>` until
  the installed compose removes project dangling images itself.
