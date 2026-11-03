# Cubeherd – Architecture

A lean web dashboard for [itzg/docker-minecraft-server](https://github.com/itzg/docker-minecraft-server).
Guiding principle: the dashboard runs alongside the Minecraft servers and must not take resources away from them.

## Core decisions

| Topic | Decision | Why |
|---|---|---|
| Language / delivery | A single Go binary, templates and assets via `embed` | ~10–25 MB RAM, no runtime dependencies |
| Frontend | Server-side HTML (`html/template`) + htmx + SSE extension, vendored | No build step, a few KB of JS |
| Dialogs | One custom confirm dialog (`cubeConfirm` in `app.js`) | Never `window.confirm`: `hx-confirm` is intercepted via `htmx:confirm`; title, button, tone and note come from `data-dlg-*` attributes |
| Dependencies | Standard library + `golang.org/x/crypto/bcrypt` | The Docker Engine API is called directly over the socket |
| State | One folder per server, no database | Servers keep working with `docker compose` without the dashboard; the dashboard only stores settings and the node list |
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

## Agent

`cubeherd agent` serves the node API of its host and runs the scheduler, so backups never depend on the dashboard being up. `remote.Client` implements the whole
`node.Node` interface against it (checked at compile time), which makes a remote host interchangeable with the in-process manager.

**Transport and trust.** The dashboard opens every connection. HTTPS with TLS 1.3 only: the agent creates a self-signed ECDSA certificate in
`$CUBEHERD_DATA/agent/` on first start and prints its SHA-256 fingerprint; the client pins that fingerprint instead of trusting a CA (`remote.Probe` fetches it
for trust on first use). Each request carries `Authorization: Bearer <token>` and `X-Cubeherd-API: <major>`. The token comes from `CUBEHERD_AGENT_TOKEN` or is
generated on the first start (printed once, only its SHA-256 is stored in `agent/token.sha256`); failed attempts are throttled per client IP like the dashboard login.
Listen address: `CUBEHERD_AGENT_LISTEN` (default `:8421`).

**Endpoints.** Identity and host (`/v1/info`, `/v1/host`, `/v1/docker`); server definitions and lifecycle (`/v1/servers[/{id}]`, `start|stop|restart|update|forget`, ports,
discover/import, `/v1/paths`); console (`rcon`, `players`, `perf`); live data (`container`, `stats`, `logs`, `ops`); files (`files`, `files/stat|text|content`, `data`,
`world.zip`, `world/stage|import`); mods (`addons`); backups (`backups`, `…/{name}/download|verify|upload|restore`, `backups/health|next`, `/v1/backups/remote`,
`/v1/scheduler/wake`); settings (`PUT /v1/config`).

**Streams.** Logs, stats and action output are newline-delimited JSON that is flushed per value and ends when the caller disconnects. File, world and archive
bodies are proxied byte for byte with no temp file. A download that fails before the first byte is a normal error response; after that the agent aborts the
connection and the client also compares the received length, so a truncated ZIP or archive is never mistaken for a complete one.

**What crosses the wire carefully.**
- The dashboard never names a host path. World import unpacks only the ZIP the agent staged itself, file paths are cleaned and resolved on the agent, and
  `Paths` reports locations for display only.
- Calls that change a definition (create, apply, update, addon upload and delete) return the saved definition, and the client replaces the caller's copy, as
  the in-process manager does by mutating it.
- Errors keep their meaning through a code (`busy`, `notfound`, `dockernotfound`, `outside`, `noworld`, `notexist`), so callers can still use `errors.Is`.
- `PUT /v1/config` carries only what an agent needs for its own work (time zone, language, CurseForge key, external backup storage). The admin password
  and session secret never leave the dashboard.

## Nodes

The dashboard manages any number of hosts. `internal/nodes.Registry` knows the embedded local node (the in-process manager, off with `CUBEHERD_LOCAL=false`) and the
remote agents from the settings (`nodes` in `cubeherd.json`, token and pinned fingerprint included, file mode 0600).

- **Bounded waits.** `Refresh` asks all nodes in parallel with a 2 s limit and returns for each one its status, host resources and servers. A node that does not answer keeps its
  last known servers, marked `unknown`, so one dead host never stalls a page. Results younger than a few seconds are reused (sidebar, live overview).
  Remote answers are only fetched while a page is open, as before.
- **Status.** online, outdated (reachable, other version than the dashboard, never for `dev` builds) or offline with a reason (unreachable, token rejected, certificate changed,
  API version). Requests to a node that is known to be offline are refused at once instead of timing out per call.
- **Settings.** What an agent needs for its own work (time zone, language, CurseForge key, external backup storage, port range) is pushed on connect and after every change; the admin password never is.
  Port range, bind addresses and default bind address are per node (Nodes page); the local node uses the global settings.
- **URLs.** `/n/<node>/s/<id>/...` is the canonical form. A middleware maps `/n/<node>/...` onto the plain routes (`/n/x/s/...` = `/servers/...`) and remembers the node; handlers registered with `h` run on a
  per-request copy of `App` whose `M` is that node (the default node for old URLs), `p` handlers are node-agnostic. Templates keep writing `/servers/...`; `scopeLinks` rewrites those attributes on node pages
  in one place. Pages that mix nodes (overview, sidebar) name the node per link (`srv`), and table rows get unique ids (`row-<node>-<id>`).
- **Wizard.** `/servers/new` is the placement step: pick a node (skipped when there is only one), then the usual wizard on `/n/<node>/s/new`.

## Accounts and access control

An opt-in mode (`config.Settings.Accounts`); without it nothing here runs and no database file exists. With it, `internal/access` owns `cubeherd.db` (SQLite through the pure Go driver `modernc.org/sqlite`, so the build stays `CGO_ENABLED=0`; WAL mode). Servers, backups and node settings are untouched and stay in their folders and in `cubeherd.json`.

- **Model.** A permission is one kind of action (`server.power`, `files.write`, …, catalogue in `access/perms.go`). Each has a *lowest scope* (server, node or global) it can be granted on; a grant at a narrower scope than that has no effect. A role is a set of permissions (built-in roles are rewritten from code on every start, custom roles live in the database). A grant is (user or group, role, scope, optional expiry); scopes are global, node and server, and a wider grant covers everything below it. Rights are the union of all grants, there are no deny rules.
- **One question.** `Principal.Can(perm, node, server)` answers every request, for the route guard and for the UI. `nil` is the single-user mode, where everything is allowed, so one code path serves both modes.
- **Routes.** `web/access.go` registers every route through `router.handle(pattern, rule, handler)`; the rule names the permission and where the scope comes from (global, the node in the URL, the server in the URL, anywhere). The mux is not reachable otherwise, and `TestEveryRouteHasARule` fails the build if a route bypasses the table, and `TestRoutesRefuseAnAccountWithoutGrants` calls each guarded route as a user with no rights. A server the user may not see answers 404.
- **UI.** `Principal` rides on the request context. `refresh()` drops servers (and nodes) the user cannot see from the overview, the sidebar and the live streams; each `api.View` carries `Can` flags that the templates use to hide buttons and tabs. The check on the server is the only authority.
- **Guard rails.** Hand out only what you hold on that scope; change an account only if you hold everything it holds; define roles only from permissions you hold globally; built-in roles are read-only; every change that could remove the last enabled owner is rolled back inside its transaction.
- **Sessions.** Opaque random tokens, only their SHA-256 is stored; revoked at once by disabling, password changes and "sign out everywhere". Sign-in limits apply per IP and per user name. CSRF protection is unchanged (same-origin check plus `SameSite=Strict`).
- **Audit.** `access.Store.Record` appends to a hash chain (each row hashes its content and the previous hash) under a mutex, in the same code path as the check. Routes carry an audit action and a target; the guard records it after the handler with the outcome (`ok`, `fail` by status or failure toast, `denied`). Reads are not logged, downloads are. Retention (default 365 days) prunes from the start and leaves a note, and the chain stays verifiable. Entries can be scoped: an `audit.view` held on a node or server only shows entries of that scope.
- **Single sign-on.** `access/oidc.go` implements discovery, the authorization code flow with PKCE, and ID token validation (RS256 and ES256 against the provider's keys, issuer, audience, expiry, nonce) with the standard library. Attempts are bound to the browser by state and a cookie. Client secrets are encrypted with AES-GCM; the key is `access.key` next to the database. Providers can restrict e-mail domains and Workspace domains, link by verified e-mail, create accounts and map a claim to groups.
- **Operations.** `cubeherd user reset-owner [name]` and `cubeherd db backup <file>` work on the database directly.

## Statistics

Optional per node (`config.Settings.Metrics` for the embedded node and for an agent, `config.Node.Metrics` on the dashboard side, pushed to the agent with the other settings; `nil` leaves an agent to its own setting or `CUBEHERD_METRICS`). Nothing runs while it is off.

- **Recorder** (`manager/metrics.go`). One goroutine, started only while on: every minute it takes `Views`, the host reading and, for each really running server (4 at a time), the Docker counters (CPU share from the difference of two readings; a restarted container or a backwards counter is skipped), the player count and TPS/MSPT over RCON. Paused, stopped, starting, busy and crashed servers only get an `up` flag, so autopause and autostop are untouched. Start, stop, restart and update events are written by the manager's actions, crashes by the sampler.
- **Store** (`internal/metrics`). `metrics.db` next to the agent state: `samples(res, scope, ts, …)` with `res` 60, 300 or 3600, plus `events`. Samples are buffered and written every five minutes and on shutdown. An hourly job builds the five-minute and hourly rows from complete finer buckets (average, maximum, minimum for TPS) with a watermark per level, then prunes by retention but never drops rows that are not yet rolled up. A size cap stops writing. An unreadable file is set aside and recreated.
- **Queries.** `Query` picks the finest resolution whose retention covers the range and thins to at most N points, weighting averages by the number of samples. Gaps stay gaps; the charts break the line.
- **API** (additive, API v1): `GET /v1/metrics/{status,latest,node,servers/{id}}`, `DELETE /v1/metrics[/{scope}]`. `remote.ErrUnsupported` is a plain 404, which an older agent answers.
- **UI.** `web/charts.go` renders server-side SVG (hover texts in `<title>`, no script, CSP unchanged). Server → Performance loads its panel by htmx; the node page is `/n/<node>/stats`. Rights `stats.server` and `stats.node` follow the route table; the server panel also needs `server.view`.
- **Prometheus.** `GET /metrics` is public in the route table but needs the scrape token (`Settings.ScrapeTokenHash`, SHA-256, constant-time compare, failures throttled). It reads the registry's cached views and the recorder's latest samples and sends no RCON of its own except through the same cached player count the overview uses.

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
internal/auth         password, session cookie (single-user mode), login throttling
internal/metrics      statistics store (SQLite): write buffer, thinning into 1 min / 5 min / 1 h rows, retention, size cap, queries
internal/access       multi-user mode: SQLite store, permissions and roles, grants and scopes, sessions, audit log, OpenID Connect
internal/docker       minimal Engine API client (list, inspect, start/stop, logs, stats, exec, info)
internal/api          wire types shared by dashboard and agent (View, Info, fingerprint helper)
internal/agent        `cubeherd agent`: HTTPS node API with self-signed certificate and bearer token
internal/remote       agent client: pinned certificate fingerprint, Probe for trust on first use
internal/node         the Node interface, split by feature (Servers, Console, Live, Files, Addons, Backups, Host); implemented by the manager (in process) and by remote (over the agent API)
internal/server       server definition, on-disk store, Compose generation, status derivation
internal/s3           minimal S3 client (SigV4, multipart upload) for external backups, standard library only
internal/update       newest GitHub release (cached, looked up lazily on page views)
internal/sources      Mojang versions, Modrinth and CurseForge search (only on user action, cached)
internal/host         host resources (Linux: /proc + statfs, otherwise Docker /info)
internal/web          handlers, templates, SSE, static assets
```

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `CUBEHERD_DATA` | `/srv/cubeherd` as root, otherwise `./cubeherd-data` | Data directory |
| `CUBEHERD_LISTEN` | `:8420` | Web server address |
| `CUBEHERD_LOCAL` | `true` | `false`: no embedded local node, the dashboard needs no Docker socket |
| `CUBEHERD_ACCOUNTS` | `false` | `true`: multi-user mode (accounts, roles, audit log in `cubeherd.db`) |
| `CUBEHERD_METRICS` | `false` | Record statistics on this node (`metrics/metrics.db`) |
| `CUBEHERD_AUDIT_STDOUT` | `false` | Also write every audit entry as a JSON line to stdout |
| `CUBEHERD_AGENT_TOKEN` | generated on first start | Agent mode: the bearer token (only its hash is stored when generated) |
| `CUBEHERD_AGENT_LISTEN` | `:8421` | Agent mode: address of the node API |
| `DOCKER_HOST` | `unix:///var/run/docker.sock`, falling back to `~/.docker/run/docker.sock` | Docker socket |

## Scope

- **MVP (v0.1):** create (software, Modrinth, CurseForge), start/stop/restart, console with RCON, live stats,
  settings, players (whitelist/ops/bans), updates (MC version, modpack version, image), uninstall,
  adopting existing itzg containers, global settings.
- **v2:** backups, mods/plugins, file browser, CPU limit and autostop – all done.
- **v3:** several hosts (dashboard plus agents) – done, see Agent and Nodes below.

## Idle & limits

- "When idle" is a choice: pause (`ENABLE_AUTOPAUSE`, wakes up on connect), stop (`ENABLE_AUTOSTOP` with
  `AUTOSTOP_TIMEOUT_EST/INIT`, also frees the RAM) or keep running. According to itzg the first two are mutually exclusive.
- Autostop forces `restart: "no"`, otherwise Docker would immediately restart the stopped server. The dashboard detects it via
  `[Autostop] Stopping Java process` in the last log lines (cached per container exit).
- CPU limit via `cpus:` in the Compose file (Docker `NanoCpus`).

## Files & worlds

- File browser on `/data`: servers with a host folder are accessed directly through the file system (symlinks that lead outside are rejected),
  servers with a Docker volume via `docker exec` or `docker run --rm` with the server image. Paths are passed to the shell as environment variables.
- The container can plant anything in `/data`, so reads and writes of host-folder servers never trust a path or its metadata:
  every write (editor, upload, addons, deletes) resolves through `hostPath`; the temp file `<name>.part` is removed and created with `O_EXCL`, and mode and owner are set on the open handle;
  reads open files non-blocking and accept regular files only (no FIFOs or devices), and `ReadText`/`ReadDataFile` cap the bytes read (1 MB), not just the size `stat` reports.
- Routes reachable without a session (`POST /login`, `/setup`) get a 64 KB body limit and a 15 s read deadline; the server-wide read timeout stays off because world uploads can take hours.
  `/setup` hands out a session only to the request whose password was actually stored.
- Editor for UTF-8 text files up to 1 MB; larger or binary files can only be downloaded. Windows line endings are normalized on save.
- World download: `tar` of the world folders (`level-name`, `_nether`, `_the_end`) from the container, converted on the fly to a ZIP in the dashboard
  (region files are stored uncompressed, since they are already compressed). If the server is running, `save-all flush` is run first.
- World upload: the ZIP is streamed next to the server, the folder containing `level.dat` is located (e.g. "My World/"), the server is stopped, backed up,
  the old world is removed and the contents are unpacked into the container as a tar (owner 1000). ZIP slip, `__MACOSX` and symlinks are discarded.
  The browser shows the upload with a progress bar (bytes, speed, remaining time, cancel) driven by htmx's `htmx:xhr:progress`;
  the import then reports file count, size and extraction progress in 10 % steps in the console log.

## Network & updates

- Port publishing: each server has an optional `bindIp`; the compose entry becomes `ip:port:25565` (IPv6 in brackets), empty means all interfaces.
  The selectable addresses are a list in the global settings (the host's own addresses cannot be discovered reliably from inside a container),
  plus a default for new servers. The port must stay unique across servers regardless of the address.
- Update notice: the first page view (and then every 6 hours, 30 minutes after a failed lookup) starts one background request to
  `api.github.com/repos/matserix/cubeherd/releases/latest`; the banner appears on the following page view. The GitHub release notes are the patch notes
  (shown as plain text), so they have to be written for users. Dismissing stores the tag; the lookup can be turned off in the settings. `dev` builds never show a banner.

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
- The next run is searched after the newest scheduled backup or attempt, and for a server that never had one after its creation. (Anchoring at
  "now" would skip the very first run for good: the timer fires a moment after the planned time and "the next run after now" is tomorrow.)
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
  on Paper and Purpur, `tps` on Folia (health report; the worst region rates the server, the busiest regions are listed), `tick query` on
  Vanilla, Fabric and Quilt (Minecraft 1.20.3 and newer, adds the 95th percentile). An unknown or unreadable reply is shown as such;
  a RCON timeout (a server stuck in one very long tick) keeps the last values on screen, dimmed, with a warning.
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
