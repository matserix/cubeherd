# Cubeherd

A lean web dashboard for [itzg/docker-minecraft-server](https://github.com/itzg/docker-minecraft-server):
create, start, update, manage and uninstall Minecraft servers (including modpacks from Modrinth and CurseForge).

Cubeherd runs on the same host as the Minecraft servers and takes next to nothing from them:
a single Go binary, around 10 MB of RAM and 0 % CPU when idle, no database. Live data only flows while a page is open.

## Getting started

### As a container (recommended)

```bash
sudo mkdir -p /srv/cubeherd
docker compose -f deploy/compose.yml up -d --build
```

Then open `http://<host>:8420` and set the admin password on first visit.

The data directory must have **the same path** inside the container as on the host (`/srv/cubeherd:/srv/cubeherd`),
because Docker resolves the Minecraft servers' bind mounts on the host.

### As a binary

Requirement: Docker with the Compose plugin on the host.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o cubeherd ./cmd/cubeherd
sudo CUBEHERD_DATA=/srv/cubeherd ./cubeherd
```

| Variable | Default | Meaning |
|---|---|---|
| `CUBEHERD_DATA` | `/srv/cubeherd` as root, otherwise `./cubeherd-data` | Data directory |
| `CUBEHERD_LISTEN` | `:8420` | Web server address |
| `DOCKER_HOST` | `/var/run/docker.sock`, falling back to `~/.docker/run/docker.sock` | Docker socket |
| `TZ` | `UTC` | Default time zone, as long as none is chosen in the settings |

## Security

Anyone who can operate Cubeherd controls Docker and therefore, in practice, the entire host.
Only expose Cubeherd on your LAN or via VPN, never directly to the internet.
It has a single admin account (bcrypt), signed session cookies (`SameSite=Strict`), login throttling and CSRF protection.

## Features

- **Create:** Vanilla, Paper, Purpur, Folia, Fabric, Quilt, Forge, NeoForge, or a modpack from Modrinth or CurseForge.
  The Java image is derived from Mojang's version data (e.g. `java25` for Minecraft 26.x).
- **Operation:** start, stop, restart, status including autopause and crash cause, live console with RCON, CPU/RAM, online players.
- **Settings:** world, RAM, Aikar flags, autopause, port, any additional itzg variables.
- **Players:** whitelist, operators, bans.
- **Updates:** Minecraft version, modpack version, itzg image.
- **Uninstall:** keep the data or delete everything.
- **Adopt** existing itzg containers.
- **Backups:** manual, scheduled, automatic before updates, restore, download.
- **Mods & plugins:** search Modrinth, CurseForge and Spigot, add and remove entries (itzg installs them on startup), upload your own JARs.
  Also works on top of modpacks.
- **Files & worlds:** file browser with editor, upload, download and delete; export a world as a ZIP and import one.
- **Idle & limits:** pause or fully stop when nobody is playing; per-server CPU limit.
- **Language & time zone:** UI in German or English, time zone for schedules, dates and server logs.

Each server lives in the data directory as `servers/<id>/compose.yml` and can also be run with `docker compose` without Cubeherd.
See [ARCHITECTURE.md](ARCHITECTURE.md) for details on the architecture.

## Development

```bash
go run ./cmd/cubeherd          # http://localhost:8420, data in ./cubeherd-data
go test ./...
FC_PROBE=<container> go test ./internal/docker -run Live -v   # against a running itzg container
FC_NET=1 go test ./internal/sources -run Live -v               # against Mojang/Modrinth
```

The clickable design prototype is in `prototype/index.html`.

## License

Cubeherd is licensed under the [MIT License](LICENSE). Bundled components (htmx, fonts, golang.org/x/crypto) and their
licenses are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Cubeherd is not an official Minecraft product and is
not affiliated with Mojang Studios or Microsoft.
