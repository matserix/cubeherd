# Forgecraft – Architektur

Schlankes Web-Dashboard für [itzg/docker-minecraft-server](https://github.com/itzg/docker-minecraft-server).
Leitlinie: Das Dashboard läuft neben den Minecraft-Servern und darf ihnen keine Ressourcen wegnehmen.

## Grundentscheidungen

| Thema | Entscheidung | Warum |
|---|---|---|
| Sprache / Auslieferung | Ein Go-Binary, Templates und Assets per `embed` | ~10–25 MB RAM, keine Laufzeit-Abhängigkeiten |
| Frontend | Serverseitiges HTML (`html/template`) + htmx + SSE-Extension, vendored | Kein Build-Schritt, wenige KB JS |
| Abhängigkeiten | Standardbibliothek + `golang.org/x/crypto/bcrypt` | Docker-Engine-API wird direkt über den Socket angesprochen |
| Zustand | Pro Server ein Ordner, keine Datenbank | Server bleiben ohne Dashboard mit `docker compose` lauffähig |
| Schreiben (anlegen, ändern, updaten) | Erzeugte `compose.yml` + `docker compose up -d` | Compose bleibt die Wahrheit für den Container |
| Lesen (Status, Logs, Stats) | Docker Engine API (`/containers/...`) | Kein CLI-Prozess pro Abfrage |
| RCON | `docker exec <ctr> rcon-cli <cmd>` | Kein RCON-Port nach außen, Passwort verwaltet das Image |
| Live-Daten | SSE, nur solange eine Server-Seite offen ist | Kein Hintergrund-Polling |
| Auth | Ein Admin, bcrypt-Hash, HMAC-signiertes Session-Cookie (`SameSite=Strict`) | Kein Session-Store nötig |
| CSRF | Mutierende Requests brauchen `HX-Request` oder gleiche Origin | Ausreichend für LAN/VPN |

## Verzeichnisstruktur auf dem Host

```
$FORGECRAFT_DATA/               # Standard: /srv/forgecraft (root) bzw. ./forgecraft-data
├── forgecraft.json             # globale Einstellungen inkl. Passwort-Hash
└── servers/
    └── <id>/
        ├── forgecraft.json     # Server-Definition (Quelle der Wahrheit)
        ├── compose.yml         # daraus erzeugt
        └── data/               # /data im Container (Welt, Configs, Mods)
```

Läuft Forgecraft selbst im Container, muss `$FORGECRAFT_DATA` unter **demselben Pfad** gemountet sein,
weil Compose relative Pfade clientseitig auflöst und der Docker-Daemon sie auf dem Host interpretiert.

## Status-Modell

| Anzeige | Quelle |
|---|---|
| Startet | laufende Aktion oder `State.Health.Status == starting` |
| Läuft | `State.Running` und healthy |
| Pausiert | `State.Running` und Datei `data/.paused` existiert (itzg-Autopause per SIGSTOP, Docker sieht das nicht) |
| Gestoppt | beendet mit Exit-Code 0 / 143 |
| Abgestürzt | beendet mit anderem Exit-Code oder `restarting`; Grund aus den letzten Log-Zeilen |
| Nicht erstellt | Definition vorhanden, Container fehlt |

## Pakete

```
cmd/forgecraft        main: Konfiguration aus Env, HTTP-Server starten
internal/config       globale Einstellungen (JSON, atomar geschrieben)
internal/auth         Passwort, Session-Cookie, Login-Drosselung
internal/docker       minimaler Engine-API-Client (list, inspect, start/stop, logs, stats, exec, info)
internal/server       Server-Definition, Store auf Platte, Compose-Erzeugung, Status-Ableitung
internal/sources      Mojang-Versionen, Modrinth- und CurseForge-Suche (nur auf Nutzeraktion, gecacht)
internal/host         Host-Ressourcen (Linux: /proc + statfs, sonst Docker /info)
internal/web          Handler, Templates, SSE, statische Assets
```

## Umgebungsvariablen

| Variable | Standard | Bedeutung |
|---|---|---|
| `FORGECRAFT_DATA` | `/srv/forgecraft` als root, sonst `./forgecraft-data` | Datenverzeichnis |
| `FORGECRAFT_LISTEN` | `:8420` | Adresse des Web-Servers |
| `DOCKER_HOST` | `unix:///var/run/docker.sock`, Fallback `~/.docker/run/docker.sock` | Docker-Socket |

## Umfang

- **MVP (v0.1):** Anlegen (Software, Modrinth, CurseForge), Start/Stop/Neustart, Konsole mit RCON, Live-Stats,
  Einstellungen, Spieler (Whitelist/OPs/Bans), Updates (MC-Version, Modpack-Version, Image), Deinstallieren,
  Übernahme bestehender itzg-Container, globale Einstellungen.
- **v2:** Mods/Plugins einzeln, Datei-Browser, Backups, CPU-Limits, Autostop.

## Erkenntnisse aus den Live-Tests

- **Java-Version:** Minecraft 26.x braucht Java 25. Forgecraft liest die Java-Version aus Mojangs Versionsdaten und wählt das passende itzg-Tag (`java8`/`java17`/`java21`/`java25`).
- **Autopause + Watchdog:** Nach dem Aufwachen sieht der Server eine riesige Tick-Lücke; der Paper-Watchdog beendet ihn dann.
  Forgecraft setzt bei Autopause daher `MAX_TICK_TIME=-1` und bei Paper/Purpur/Folia zusätzlich `JVM_DD_OPTS=disable.watchdog:true`.
- **Pausierte Server:** RCON-Befehle wecken sie nicht (rcon-cli läuft über `lo`, knockd lauscht auf `eth0`), deshalb lehnt die Konsole sie ab.
  `docker stop` funktioniert dagegen: das Image setzt den Prozess fort und speichert die Welt.
- **RCON-Kosten:** Jede Abfrage startet `rcon-cli` im Container und schreibt zwei Zeilen ins Server-Log. Spielerlisten werden 15 s gecacht,
  diese Log-Zeilen blendet die Konsole aus.
- **Start/Neustart** laufen über `docker compose up -d`, damit gespeicherte Einstellungen beim nächsten Start greifen (Compose erstellt bei Abweichung neu).
- **Arbeitsspeicher:** Der eingestellte RAM ist das Container-Limit (`mem_limit`). Der Java-Heap (`MEMORY`) bekommt davon weniger:
  Vanilla/Paper `max(1 GB, 20 %)` Puffer, Modpacks/Mod-Loader `max(2 GB, 30 %)`. Gemessen mit ATM10: rund 1,7 GB außerhalb des Heaps.
  Vorher war `MEMORY` der volle Wert ohne Limit; Heap plus Nicht-Heap sprengte die Docker-VM und führte zu Neustart-Schleifen.
- **Startphase:** Solange itzg noch Dateien lädt (`[init]`, `[mc-image-helper]`), schlägt der Healthcheck fehl. Forgecraft liest die neuen
  Log-Zeilen des laufenden Containers inkrementell und zeigt dann „Wird eingerichtet“ bzw. „Startet“ statt „Reagiert nicht“.
- **`MALLOC_ARENA_MAX=2`** wird bei Modpack- und Mod-Servern gesetzt. ATM10, gemessen jeweils 234 s nach dem Start:
  Speicher außerhalb des Heaps 1,63 GiB → 1,46 GiB (−0,17 GiB, ~10 %).
