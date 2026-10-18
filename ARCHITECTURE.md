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
- **v2:** Backups, Mods/Plugins, Datei-Browser, CPU-Limit und Autostop – alles fertig.

## Leerlauf & Limits

- „Bei Leerlauf“ ist eine Wahl: pausieren (`ENABLE_AUTOPAUSE`, wacht beim Verbinden auf), stoppen (`ENABLE_AUTOSTOP` mit
  `AUTOSTOP_TIMEOUT_EST/INIT`, gibt auch den RAM frei) oder weiterlaufen. Beides schließt sich laut itzg aus.
- Autostop erzwingt `restart: "no"`, sonst würde Docker den gestoppten Server sofort neu starten. Erkennung im Dashboard über
  `[Autostop] Stopping Java process` in den letzten Log-Zeilen (gecacht pro Container-Ende).
- CPU-Limit über `cpus:` in der Compose-Datei (Docker `NanoCpus`).

## Dateien & Welten

- Datei-Browser auf `/data`: Server mit Host-Ordner direkt über das Dateisystem (Symlinks, die herausführen, werden abgewiesen),
  Server mit Docker-Volume über `docker exec` bzw. `docker run --rm` mit dem Server-Image. Pfade gehen als Umgebungsvariable in die Shell.
- Editor für UTF-8-Textdateien bis 1 MB; größere oder binäre Dateien nur zum Herunterladen. Windows-Zeilenenden werden beim Speichern normalisiert.
- Welt-Download: `tar` der Welt-Ordner (`level-name`, `_nether`, `_the_end`) aus dem Container, im Dashboard on-the-fly zu ZIP
  (Region-Dateien unkomprimiert gespeichert, sie sind schon komprimiert). Läuft der Server, wird vorher `save-all flush` ausgeführt.
- Welt-Upload: ZIP wird neben den Server gestreamt, der Ordner mit `level.dat` gesucht (z. B. „Meine Welt/“), Server gestoppt, gesichert,
  alte Welt entfernt und der Inhalt als tar in den Container entpackt (Besitzer 1000). ZIP-Slip, `__MACOSX` und Symlinks werden verworfen.

## Mods & Plugins

- Deklarativ: Forgecraft pflegt `MODRINTH_PROJECTS` (mit `MODRINTH_DOWNLOAD_DEPENDENCIES=required`), `CURSEFORGE_FILES` und `SPIGET_RESOURCES`.
  itzg lädt beim Start passend zu Loader und Version herunter und hält nicht gepinnte Einträge aktuell.
- Entfernen: Für eine Quelle, die schon einmal benutzt wurde, setzt Forgecraft die Variable explizit leer. Nur dann räumt itzg die von
  ihm installierten Dateien auf; eine fehlende Variable lässt sie liegen. Getestet: Paper + Modrinth-Plugin, ATM10 (CurseForge-Pack) + Modrinth-Mod,
  die Modpack-Mods bleiben dabei unberührt. Spigot-Plugins räumt itzg nicht auf.
- Hochgeladene JARs landen direkt in `mods/` bzw. `plugins/` und werden in der Definition gemerkt; nur sie lassen sich im Dashboard löschen.
- Suche: Modrinth (`project_type:mod` mit Loader, Version und Server-Seite bzw. `project_type:plugin`), CurseForge (eigener API-Key),
  Spiget (nur kostenlose, direkt herunterladbare Ressourcen; die API ist langsam, Abbruch nach 8 s).

## Backups

- Archive liegen unter `servers/<id>/backups/<YYYYMMDD-HHMMSS>_<anlass>.tar.zst` (Anlass: manual, schedule, update, restore).
- Läuft der Server: `save-off` → `save-all flush` → `docker exec … nice ionice tar | zstd -3 -T1` → `save-on`.
  Gestoppt oder pausiert: dasselbe per `docker run --rm` mit dem Server-Image und `/data` schreibgeschützt.
- Ausgeschlossen ist, was itzg neu lädt: Logs, Caches, Bibliotheken, Server-JARs; bei Modpacks auch `mods/`.
- Wiederherstellen stoppt den Server, sichert den aktuellen Stand, löscht nur die im Archiv enthaltenen Einträge und entpackt.
- Der Zeitplan ist ein einzelner Timer im Dashboard, der bis zum nächsten fälligen Backup schläft. Verpasste Läufe werden 2 Minuten nach dem Start nachgeholt.
  Zeitzone über `TZ` (Zonendaten sind ins Binary eingebettet).
- Automatische Backups werden nach der Aufbewahrungsregel gelöscht, manuelle nie.

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
