# Cubeherd

Schlankes Web-Dashboard für [itzg/docker-minecraft-server](https://github.com/itzg/docker-minecraft-server):
Minecraft-Server (auch Modpacks von Modrinth und CurseForge) anlegen, starten, aktualisieren, verwalten und deinstallieren.

Cubeherd läuft auf demselben Host wie die Minecraft-Server und nimmt ihnen kaum etwas weg:
ein Go-Binary, im Leerlauf rund 10 MB RAM und 0 % CPU, keine Datenbank. Live-Daten fließen nur, solange eine Seite offen ist.

## Starten

### Als Container (empfohlen)

```bash
sudo mkdir -p /srv/cubeherd
docker compose -f deploy/compose.yml up -d --build
```

Dann `http://<host>:8420` öffnen und beim ersten Aufruf das Admin-Passwort festlegen.

Das Datenverzeichnis muss im Container unter **demselben Pfad** wie auf dem Host liegen (`/srv/cubeherd:/srv/cubeherd`),
weil Docker die Bind-Mounts der Minecraft-Server auf dem Host auflöst.

### Als Binary

Voraussetzung: Docker mit Compose-Plugin auf dem Host.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o cubeherd ./cmd/cubeherd
sudo CUBEHERD_DATA=/srv/cubeherd ./cubeherd
```

| Variable | Standard | Bedeutung |
|---|---|---|
| `CUBEHERD_DATA` | `/srv/cubeherd` als root, sonst `./cubeherd-data` | Datenverzeichnis |
| `CUBEHERD_LISTEN` | `:8420` | Adresse des Web-Servers |
| `DOCKER_HOST` | `/var/run/docker.sock`, Fallback `~/.docker/run/docker.sock` | Docker-Socket |
| `TZ` | `UTC` | Standard-Zeitzone, solange in den Einstellungen keine gewählt ist |

## Sicherheit

Wer Cubeherd bedienen kann, steuert Docker und damit praktisch den ganzen Host.
Cubeherd deshalb nur im LAN oder per VPN erreichbar machen, nie direkt im Internet.
Es gibt ein Admin-Konto (bcrypt), signierte Session-Cookies (`SameSite=Strict`), eine Login-Drosselung und einen CSRF-Schutz.

## Funktionen

- **Anlegen:** Vanilla, Paper, Purpur, Folia, Fabric, Quilt, Forge, NeoForge oder ein Modpack von Modrinth bzw. CurseForge.
  Das Java-Image wird aus Mojangs Versionsdaten abgeleitet (z. B. `java25` für Minecraft 26.x).
- **Betrieb:** Start, Stopp, Neustart, Status inklusive Autopause und Absturzursache, Live-Konsole mit RCON, CPU/RAM, Online-Spieler.
- **Einstellungen:** Spielwelt, RAM, Aikar-Flags, Autopause, Port, beliebige zusätzliche itzg-Variablen.
- **Spieler:** Whitelist, Operatoren, Bans.
- **Updates:** Minecraft-Version, Modpack-Version, itzg-Image.
- **Deinstallieren:** Daten behalten oder alles löschen.
- **Übernahme** bestehender itzg-Container.
- **Backups:** manuell, per Zeitplan, automatisch vor Updates, Wiederherstellen, Herunterladen.
- **Mods & Plugins:** Suche bei Modrinth, CurseForge und Spigot, eintragen und austragen (itzg installiert beim Start), eigene JARs hochladen.
  Funktioniert auch zusätzlich zu Modpacks.
- **Dateien & Welten:** Datei-Browser mit Editor, Hochladen, Herunterladen, Löschen; Welt als ZIP exportieren und einspielen.
- **Leerlauf & Limits:** pausieren oder ganz stoppen, wenn niemand spielt; CPU-Limit pro Server.
- **Sprache & Zeitzone:** Oberfläche auf Deutsch oder Englisch, Zeitzone für Zeitpläne, Datumsangaben und Server-Logs.

Jeder Server liegt als `servers/<id>/compose.yml` im Datenverzeichnis und lässt sich auch ohne Cubeherd mit `docker compose` betreiben.
Details zur Architektur stehen in [ARCHITECTURE.md](ARCHITECTURE.md).

## Entwicklung

```bash
go run ./cmd/cubeherd          # http://localhost:8420, Daten in ./cubeherd-data
go test ./...
FC_PROBE=<container> go test ./internal/docker -run Live -v   # gegen einen laufenden itzg-Container
FC_NET=1 go test ./internal/sources -run Live -v               # gegen Mojang/Modrinth
```

Der klickbare Design-Prototyp liegt in `prototype/index.html`.

## Lizenz

Cubeherd steht unter der [MIT-Lizenz](LICENSE). Mitgelieferte Komponenten (htmx, Schriften, golang.org/x/crypto) und ihre
Lizenzen sind in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) aufgeführt. Cubeherd ist kein offizielles Minecraft-Produkt und
steht in keiner Verbindung zu Mojang Studios oder Microsoft.
