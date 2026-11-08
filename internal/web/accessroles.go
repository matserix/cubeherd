package web

import (
	"net/http"
	"strconv"
	"strings"

	"cubeherd/internal/access"
)

// permText is what the role editor shows for each permission. The keys are German source texts like every other
// string; en.json carries the translations (TestPermissionsAreDescribed checks that none is missing).
var permText = map[access.Perm][2]string{
	access.ServerView:     {"Server ansehen", "Status, Spieler, Logs und Metriken sehen."},
	access.ServerPower:    {"Starten und stoppen", "Server starten, stoppen, neu starten, pausieren und fortsetzen."},
	access.ServerConsole:  {"Konsole lesen", "Die Live-Konsole ansehen."},
	access.ServerCommand:  {"Befehle senden", "Konsolen- und RCON-Befehle senden. Das entspricht fast Operator-Rechten im Spiel."},
	access.ServerPlayers:  {"Spieler verwalten", "Whitelist, Operatoren und Sperren."},
	access.ServerSettings: {"Einstellungen ändern", "Spielregeln, Speicher, Ports und Umgebungsvariablen."},
	access.ServerUpdate:   {"Version aktualisieren", "Minecraft-Version oder Modpack aktualisieren oder wechseln."},
	access.ServerCreate:   {"Server anlegen", "Neue Server anlegen oder bestehende Container übernehmen."},
	access.ServerDelete:   {"Server deinstallieren", "Server samt Daten entfernen."},
	access.FilesRead:      {"Dateien lesen", "Dateien ansehen und herunterladen. Konfigurationen können Geheimnisse wie das RCON-Passwort enthalten."},
	access.FilesWrite:     {"Dateien ändern", "Dateien bearbeiten, hochladen und löschen."},
	access.WorldExport:    {"Welt herunterladen", "Die Welt als ZIP herunterladen."},
	access.WorldImport:    {"Welt ersetzen", "Die Welt durch ein hochgeladenes ZIP ersetzen."},
	access.AddonsManage:   {"Mods und Plugins", "Mods und Plugins suchen, installieren, hochladen und entfernen."},
	access.BackupsView:    {"Backups ansehen", "Backups auflisten, prüfen und herunterladen."},
	access.BackupsCreate:  {"Backups erstellen", "Backups starten, Zeitplan ändern, in den externen Speicher hochladen."},
	access.BackupsRestore: {"Backups wiederherstellen", "Welt und Konfiguration aus einem Backup wiederherstellen."},
	access.BackupsDelete:  {"Backups löschen", "Backups endgültig löschen."},
	access.StatsServer:    {"Server-Statistik", "Den Verlauf eines Servers sehen: Auslastung, Spieler, TPS und Ereignisse."},
	access.StatsNode:      {"Node-Statistik", "Den Verlauf eines Nodes sehen: CPU, Arbeitsspeicher, Festplatte und Server."},
	access.NodesView:      {"Nodes ansehen", "Auslastung und Zustand eines Nodes sehen (CPU, RAM, Festplatte, Agent). Ohne dieses Recht sieht man nur die erlaubten Server."},
	access.NodesManage:    {"Nodes verwalten", "Nodes hinzufügen, ändern und entfernen (Tokens!). Gilt nur für das ganze Dashboard."},
	access.SettingsManage: {"Globale Einstellungen", "S3, CurseForge-Key, Zeitzone, Sprache. Gilt nur für das ganze Dashboard."},
	access.UsersManage:    {"Benutzer verwalten", "Konten, Gruppen und Zuweisungen. Gilt nur für das ganze Dashboard."},
	access.RolesManage:    {"Rollen verwalten", "Eigene Rollen anlegen und ändern. Gilt nur für das ganze Dashboard."},
	access.UsersApprove:   {"Registrierungen freigeben", "Neue Registrierungen annehmen oder ablehnen. Gilt nur für das ganze Dashboard."},
	access.SSOManage:      {"Anmeldeanbieter", "Einmalanmeldung (OpenID Connect) einrichten. Gilt nur für das ganze Dashboard."},
	access.AuditView:      {"Audit-Log ansehen", "Einträge des zugewiesenen Bereichs lesen."},
	access.AuditExport:    {"Audit-Log exportieren", "Einträge des zugewiesenen Bereichs als CSV oder JSON exportieren."},
}

// builtinDesc describes the built-in roles; the database only keeps an English text for the command line.
var builtinDesc = map[string]string{
	access.RoleOwner:     "Alles, auch Konten, Rollen, Anmeldeanbieter und Audit-Log.",
	access.RoleAdmin:     "Server, Nodes und globale Einstellungen, aber keine Konten und kein Audit-Log.",
	access.RoleOperator:  "Betreibt Server: Start und Stopp, Konsole, Einstellungen, Dateien, Mods und Backups. Kann keine Server anlegen oder löschen.",
	access.RoleModerator: "Startet und stoppt Server, verwaltet Spieler und sendet Befehle.",
	access.RoleViewer:    "Nur lesen: Status, Live-Konsole und die Liste der Backups.",
}

var groupText = map[string]string{"server": "Server", "files": "Dateien und Welt", "addons": "Mods und Plugins", "backups": "Backups", "stats": "Statistik", "admin": "Verwaltung", "access": "Zugriff"}

type permItem struct {
	access.Info
	Name, Desc string
	Checked    bool
	Scope      string
}

type permGroup struct {
	Name  string
	Items []permItem
}

func permGroups(has func(access.Perm) bool) []permGroup {
	var out []permGroup
	for _, g := range access.Groups {
		pg := permGroup{Name: groupText[g]}
		for _, i := range access.Catalog {
			if i.Group != g {
				continue
			}
			t := permText[i.ID]
			pg.Items = append(pg.Items, permItem{Info: i, Name: t[0], Desc: t[1], Checked: has(i.ID), Scope: [...]string{"Alles", "Node", "Server"}[i.Lowest]})
		}
		out = append(out, pg)
	}
	return out
}

type roleRow struct {
	access.Role
	Sensitive int
	Desc      string // German source text for built-in roles (translated in the template), free text otherwise
}

type roleEdit struct {
	access.Role
	Groups   []permGroup
	ReadOnly bool
	Clone    bool
}

func (a *App) rolesPage(w http.ResponseWriter, r *http.Request) {
	rs, _ := a.store().ListRoles()
	var rows []roleRow
	for _, ro := range rs {
		row := roleRow{Role: ro, Desc: ro.Description}
		if ro.Builtin {
			row.Desc = builtinDesc[ro.Name]
		}
		for _, pm := range ro.Perms {
			if i, ok := access.Lookup(pm); ok && i.Sensitive {
				row.Sensitive++
			}
		}
		rows = append(rows, row)
	}
	a.accessPage(w, r, "roles", accessData{Roles: rows})
}

func (a *App) roleNew(w http.ResponseWriter, r *http.Request) {
	e := &roleEdit{}
	if from := r.FormValue("from"); from != "" {
		if id, err := strconv.ParseInt(from, 10, 64); err == nil {
			if src, err := a.store().Role(id); err == nil {
				e.Role = access.Role{Name: a.t("%s (Kopie)", src.Name), Description: src.Description, Perms: src.Perms}
				e.Clone = true
			}
		}
	}
	e.Role.ID = 0
	e.Groups = permGroups(e.Role.Has)
	a.partial(w, "access", "roleForm", e)
}

func (a *App) roleEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ro, err := a.store().Role(id)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	a.partial(w, "access", "roleForm", &roleEdit{Role: ro, Groups: permGroups(ro.Has), ReadOnly: ro.Builtin})
}

func (a *App) roleSave(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	var id int64
	if r.PathValue("id") != "" {
		var ok bool
		if id, ok = id64(r); !ok {
			http.NotFound(w, r)
			return
		}
	}
	_ = r.ParseForm()
	var perms []access.Perm
	for _, v := range r.Form["perm"] {
		if access.Valid(access.Perm(v)) {
			perms = append(perms, access.Perm(v))
		}
	}
	ro := access.Role{ID: id, Name: strings.TrimSpace(r.FormValue("name")), Description: r.FormValue("description"), Perms: perms}
	if ro.Name == "" {
		a.fail(w, "Bitte gib einen Namen an.")
		return
	}
	// A role may only contain what the editor holds on the whole dashboard, and an existing role may only be
	// edited by someone who holds all of it (otherwise a role could be used to take rights away or add them).
	if !p.CanDefine(perms) {
		a.fail(w, "Du kannst nur Rechte in eine Rolle aufnehmen, die du selbst für das ganze Dashboard hast.")
		return
	}
	if id != 0 {
		old, err := st.Role(id)
		if err != nil {
			a.storeErr(w, err)
			return
		}
		if !p.CanDefine(old.Perms) {
			a.fail(w, "Diese Rolle enthält Rechte, die du nicht hast. Du kannst sie nicht ändern.")
			return
		}
	}
	if _, err := st.SaveRole(ro); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/roles")
}

func (a *App) roleDelete(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ro, err := st.Role(id)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	if !p.CanDefine(ro.Perms) {
		a.fail(w, "Diese Rolle enthält Rechte, die du nicht hast. Du kannst sie nicht ändern.")
		return
	}
	if err := st.DeleteRole(id); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/roles")
}
