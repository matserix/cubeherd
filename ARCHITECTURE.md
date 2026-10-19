# Cubeherd – Architecture

A lean web dashboard for [itzg/docker-minecraft-server](https://github.com/itzg/docker-minecraft-server).
Guiding principle: the dashboard runs alongside the Minecraft servers and must not take resources away from them.

## Core decisions

| Topic | Decision | Why |
|---|---|---|
| Language / delivery | A single Go binary, templates and assets via `embed` | ~10–25 MB RAM, no runtime dependencies |
| Frontend | Server-side HTML (`html/template`) + htmx + SSE extension, vendored | No build step, a few KB of JS |
| Dependencies | Standard library + `golang.org/x/crypto/bcrypt` | The Docker Engine API is called directly over the socket |
| State | One folder per server, no database | Servers keep working with `docker compose` without the dashboard |
| Writing (create, change, update) | Generated `compose.yml` + `docker compose up -d` | Compose remains the source of truth for the container |
| Reading (status, logs, stats) | Docker Engine API (`/containers/...`) | No CLI process per query |
| RCON | `docker exec <ctr> rcon-cli <cmd>` | No RCON port exposed, the image manages the password |
| Live data | SSE, only while a server page is open | No background polling |
| Auth | One admin, bcrypt hash, HMAC-signed session cookie (`SameSite=Strict`) | No session store needed |
| CSRF | Mutating requests need `HX-Request` or the same origin | Sufficient for LAN/VPN |

## Directory layout on the host

```
$CUBEHERD_DATA/               # default: /srv/cubeherd (root) or ./cubeherd-data
├── cubeherd.json             # global settings including the password hash
└── servers/
    └── <id>/
        ├── cubeherd.json     # server definition (source of truth)
        ├── compose.yml         # generated from it
        └── data/               # /data in the container (world, configs, mods)
```

If Cubeherd itself runs in a container, `$CUBEHERD_DATA` must be mounted at **the same path**,
because Compose resolves relative paths on the client side and the Docker daemon interprets them on the host.

## Status model

| Display | Source |
|---|---|
| Starting | running action or `State.Health.Status == starting` |
| Running | `State.Running` and healthy |
| Paused | `State.Running` and the file `data/.paused` exists (itzg autopause via SIGSTOP, which Docker does not see) |
| Stopped | exited with exit code 0 / 143 |
| Crashed | exited with any other exit code, or `restarting`; cause taken from the last log lines |
| Not created | definition exists, container missing |

## Packages

```
cmd/cubeherd        main: read configuration from env, start the HTTP server
internal/config       global settings (JSON, written atomically)
internal/auth         password, session cookie, login throttling
internal/docker       minimal Engine API client (list, inspect, start/stop, logs, stats, exec, info)
internal/server       server definition, on-disk store, Compose generation, status derivation
internal/s3           minimal S3 client (SigV4, multipart upload) for external backups, standard library only
internal/sources      Mojang versions, Modrinth and CurseForge search (only on user action, cached)
internal/host         host resources (Linux: /proc + statfs, otherwise Docker /info)
internal/web          handlers, templates, SSE, static assets
```

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `CUBEHERD_DATA` | `/srv/cubeherd` as root, otherwise `./cubeherd-data` | Data directory |
| `CUBEHERD_LISTEN` | `:8420` | Web server address |
| `DOCKER_HOST` | `unix:///var/run/docker.sock`, falling back to `~/.docker/run/docker.sock` | Docker socket |

## Scope

- **MVP (v0.1):** create (software, Modrinth, CurseForge), start/stop/restart, console with RCON, live stats,
  settings, players (whitelist/ops/bans), updates (MC version, modpack version, image), uninstall,
  adopting existing itzg containers, global settings.
- **v2:** backups, mods/plugins, file browser, CPU limit and autostop – all done.

## Idle & limits

- "When idle" is a choice: pause (`ENABLE_AUTOPAUSE`, wakes up on connect), stop (`ENABLE_AUTOSTOP` with
  `AUTOSTOP_TIMEOUT_EST/INIT`, also frees the RAM) or keep running. According to itzg the first two are mutually exclusive.
- Autostop forces `restart: "no"`, otherwise Docker would immediately restart the stopped server. The dashboard detects it via
  `[Autostop] Stopping Java process` in the last log lines (cached per container exit).
- CPU limit via `cpus:` in the Compose file (Docker `NanoCpus`).

## Files & worlds

- File browser on `/data`: servers with a host folder are accessed directly through the file system (symlinks that lead outside are rejected),
  servers with a Docker volume via `docker exec` or `docker run --rm` with the server image. Paths are passed to the shell as environment variables.
- Editor for UTF-8 text files up to 1 MB; larger or binary files can only be downloaded. Windows line endings are normalized on save.
- World download: `tar` of the world folders (`level-name`, `_nether`, `_the_end`) from the container, converted on the fly to a ZIP in the dashboard
  (region files are stored uncompressed, since they are already compressed). If the server is running, `save-all flush` is run first.
- World upload: the ZIP is streamed next to the server, the folder containing `level.dat` is located (e.g. "My World/"), the server is stopped, backed up,
  the old world is removed and the contents are unpacked into the container as a tar (owner 1000). ZIP slip, `__MACOSX` and symlinks are discarded.

## Mods & plugins

- Declarative: Cubeherd maintains `MODRINTH_PROJECTS` (with `MODRINTH_DOWNLOAD_DEPENDENCIES=required`), `CURSEFORGE_FILES` and `SPIGET_RESOURCES`.
  itzg downloads matching the loader and version on startup and keeps unpinned entries up to date.
- Removal: for a source that has been used before, Cubeherd explicitly sets the variable to empty. Only then does itzg clean up the files it
  installed; a missing variable leaves them in place. Tested: Paper + Modrinth plugin, ATM10 (CurseForge pack) + Modrinth mod,
  with the modpack's mods left untouched. itzg does not clean up Spigot plugins.
- Uploaded JARs go straight into `mods/` or `plugins/` and are recorded in the definition; only these can be deleted from the dashboard.
- Search: Modrinth (`project_type:mod` with loader, version and server side, or `project_type:plugin`), CurseForge (own API key),
  Spiget (only free, directly downloadable resources; the API is slow, aborted after 8 s).

## Backups

- Archives are stored at `servers/<id>/backups/<YYYYMMDD-HHMMSS>_<reason>.tar.zst` (reason: manual, schedule, update, restore).
- If the server is running: `save-off` → `save-all flush` → `docker exec … nice ionice tar | zstd -3 -T1` → `save-on`.
  If stopped or paused: the same via `docker run --rm` with the server image and `/data` mounted read-only.
- Excluded is whatever itzg downloads again: logs, caches, libraries, server JARs; for modpacks also `mods/`.
- Restoring stops the server, backs up the current state, deletes only the entries contained in the archive and unpacks it.
- The schedule is a single timer in the dashboard that sleeps until the next backup is due. Missed runs are caught up 2 minutes after startup.
  Time zone from the settings (otherwise `TZ`/system; zone data is embedded in the binary). It is also passed to the
  containers as `TZ` so that log timestamps match. A backup's time comes from the file date, the name is only a label –
  so changing the time zone does not shift existing backups.
- Automatic backups are deleted according to the retention rule, manual ones never.
- External storage (optional, S3-compatible): one bucket in the global settings, switched on per server in the backup plan.
  After a backup is written locally it is uploaded to `<prefix>/<server id>/<name>`. Files up to 8 MiB go in one request, larger ones as a
  multipart upload streamed from disk in 8 MiB parts (unsigned payload, so nothing is buffered in memory). An upload failure never
  deletes the local archive; it is logged and makes the operation fail visibly, but does not abort updates or restores that
  made the backup.
- "Do not keep locally" (`RemoteOnly`, needs the upload switch): after a successful upload the local archive is removed, so the archive only
  exists in the bucket. The safety copy made before a restore always stays local, and an archive fetched for a restore is removed again afterwards.
- The retention rule applies to the bucket as well: after each upload, automatic archives beyond `Keep` are deleted there, manual ones never.
- The backup tab merges local and remote listings (remote listings are cached for a minute and refreshed after uploads and deletes).
  Remote-only archives can be downloaded, restored (they are fetched into the local folder first) and deleted; deleting removes every copy.
- Credentials are stored in `cubeherd.json` (mode 0600), like the CurseForge key. The secret is never rendered back into the form.
- Verification (on by default, `NoVerify` turns it off): right after an archive is written, `tar -I zstd -tf` reads it completely in a
  short-lived container of the server image (low CPU and I/O priority, nothing is extracted). A broken archive is kept but never pruned
  against or uploaded, makes the operation fail (so an update, restore or uninstall that depended on it stops) and is marked "damaged" in
  the list. The "Check" button re-reads an archive on demand; remote-only archives are downloaded for it and removed again.
- Health (`servers/<id>/backup-health.json`, a few facts only): time of the last good backup (written, verified and uploaded as configured),
  the last failure and its cause, and the check result per archive name. The banner on top of the Backups tab is derived from it: critical
  when the newest archive is damaged, a warning when a run failed or none succeeded within the limit (default two intervals, or 25 h, or 3 days;
  never for a schedule that is off, and not for a stopped server with "only back up while running"). Archives from before the feature count as
  good until a check says otherwise. `NoWarn` hides the banner.

## Performance tab

- Tick performance comes from RCON, because spark does not answer over RCON: `neoforge tps` / `forge tps` (per dimension), `tps` + `mspt`
  on Paper and Purpur, `tps` on Folia, `tick query` on Vanilla, Fabric and Quilt (Minecraft 1.20.3 and newer, adds the 95th percentile).
  Parsers live in `internal/server/perf.go` and are tested against output captured from real containers.
- Like everything live, it only runs while the tab is open: one SSE stream samples every 5 s, keeps up to 120 samples (10 minutes) in memory
  for that page and renders the whole panel (cards, chart, dimension table) server side. Nothing is stored.
- A paused server is never queried, because RCON would not wake it. A stopped or starting server just shows why there are no values.
- Rating (`PerfLevel`): critical below 15 TPS or from 60 ms per tick, warning below 19.5 TPS or from 40 ms, otherwise fine.

## Findings from live tests

- **Java version:** Minecraft 26.x needs Java 25. Cubeherd reads the Java version from Mojang's version data and picks the matching itzg tag (`java8`/`java17`/`java21`/`java25`).
- **Autopause + watchdog:** after waking up, the server sees a huge tick gap, and the Paper watchdog then kills it.
  With autopause, Cubeherd therefore sets `MAX_TICK_TIME=-1` and, for Paper/Purpur/Folia, additionally `JVM_DD_OPTS=disable.watchdog:true`.
- **Paused servers:** RCON commands do not wake them (rcon-cli runs over `lo`, knockd listens on `eth0`), so the console rejects them.
  `docker stop`, on the other hand, works: the image resumes the process and saves the world.
- **RCON cost:** every query starts `rcon-cli` in the container and writes two lines to the server log. Player lists are cached for 15 s,
  and the console hides these log lines.
- **Start/restart** go through `docker compose up -d` so that saved settings take effect on the next start (Compose recreates the container on a mismatch).
- **Memory:** the configured RAM is the container limit (`mem_limit`). The Java heap (`MEMORY`) gets less of it:
  Vanilla/Paper `max(1 GB, 20 %)` headroom, modpacks/mod loaders `max(2 GB, 30 %)`. Measured with ATM10: about 1.7 GB outside the heap.
  Previously `MEMORY` was the full value with no limit; heap plus non-heap exceeded the Docker VM and caused restart loops.
- **Startup phase:** while itzg is still downloading files (`[init]`, `[mc-image-helper]`), the health check fails. Cubeherd reads the new
  log lines of the running container incrementally and then shows "Setting up" or "Starting" instead of "Not responding".
- **`MALLOC_ARENA_MAX=2`** is set for modpack and mod servers. ATM10, each measured 234 s after startup:
  memory outside the heap 1.63 GiB → 1.46 GiB (−0.17 GiB, ~10 %).

## Language

- German and English. German is the source language and the German text is the key; every further language is a
  JSON file in `internal/i18n/` (`<code>.json`) plus an entry in `i18n.Languages`.
- The language is a global setting (one admin). Existing installations without the setting stay German;
  on a fresh setup Cubeherd suggests the language based on `Accept-Language`.
- Each language has its own template set with its own formatting functions (decimal separator, date format, "Heute/Today");
  the SSE partial views use it too. Switching the language reloads the page.
- Messages with values are translated by the manager when they are created (`m.t("Port %d …", port)`), fixed error texts are translated
  by the web UI when they are displayed. Minecraft and itzg log lines are never translated.
- `TestCatalogsComplete` checks both directions: every text in use has a translation, and every translation is still
  in use. In addition, the format placeholders must match.
