package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"forgecraft/internal/auth"
	"forgecraft/internal/config"
	"forgecraft/internal/docker"
	"forgecraft/internal/host"
	"forgecraft/internal/manager"
	"forgecraft/internal/server"
	"forgecraft/internal/sources"
)

func selfMB() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return (ms.Sys - ms.HeapReleased) >> 20
}

// ---------------- Auth ----------------

type loginData struct {
	Setup  bool
	Action string
	Error  string
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.sess.Valid(r) {
		redirect(w, r, "/")
		return
	}
	a.render(w, r, "login", Page{Title: "Anmelden", Nav: "login", Data: loginData{Action: "/login"}})
}

func (a *App) loginPost(w http.ResponseWriter, r *http.Request) {
	if d := a.lim.Wait(r); d > 0 {
		a.render(w, r, "login", Page{Nav: "login", Data: loginData{Action: "/login", Error: fmt.Sprintf("Zu viele Versuche. Bitte %d Sekunden warten.", int(d.Seconds())+1)}})
		return
	}
	if !auth.CheckPassword(a.Cfg.Get().PasswordHash, r.FormValue("password")) {
		a.lim.Fail(r)
		time.Sleep(400 * time.Millisecond)
		a.render(w, r, "login", Page{Nav: "login", Data: loginData{Action: "/login", Error: "Passwort ist falsch."}})
		return
	}
	a.lim.Reset(r)
	a.sess.Issue(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "login", Page{Title: "Einrichten", Nav: "login", Data: loginData{Setup: true, Action: "/setup"}})
}

func (a *App) setupPost(w http.ResponseWriter, r *http.Request) {
	pw := r.FormValue("password")
	fail := func(msg string) {
		a.render(w, r, "login", Page{Nav: "login", Data: loginData{Setup: true, Action: "/setup", Error: msg}})
	}
	if len(pw) < 10 {
		fail("Das Passwort braucht mindestens 10 Zeichen.")
		return
	}
	if pw != r.FormValue("password2") {
		fail("Die Passwörter stimmen nicht überein.")
		return
	}
	hash, err := auth.HashPassword(pw)
	if err == nil {
		err = a.Cfg.Update(func(s *config.Settings) {
			if s.PasswordHash == "" { // never overwrite through a race
				s.PasswordHash = hash
			}
		})
	}
	if err != nil {
		fail("Speichern fehlgeschlagen: " + err.Error())
		return
	}
	a.sess.Issue(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	a.sess.Clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------- Übersicht ----------------

type overviewData struct {
	Host       host.Stats
	Alloc      uint64
	SelfMB     uint64
	Goroutines int
	Views      []manager.View
	Running    int
	Players    int
	Unmanaged  []manager.Unmanaged
}

func (a *App) overviewData(ctx context.Context, discover bool) overviewData {
	d := overviewData{Host: a.Host.Read(ctx), SelfMB: selfMB(), Goroutines: runtime.NumGoroutine()}
	d.Views, _ = a.M.Views(ctx, true)
	for _, v := range d.Views {
		if v.Status.Up() {
			d.Running++
			d.Alloc += uint64(v.S.MemoryGB) << 30
		}
		d.Players += len(v.Online)
	}
	if discover {
		d.Unmanaged = a.M.Discover(ctx)
	}
	return d
}

func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "overview", Page{Title: "Übersicht", Nav: "overview", Data: a.overviewData(r.Context(), true)})
}

func (a *App) overviewPartial(w http.ResponseWriter, r *http.Request) {
	a.partial(w, "overview", "ovLive", a.overviewData(r.Context(), false))
}

func (a *App) importPost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	s, err := a.M.Import(r.Context(), r.FormValue("container"), name)
	if err != nil {
		fail(w, "Übernahme nicht möglich: "+err.Error())
		return
	}
	redirect(w, r, "/servers/"+s.ID)
}

// ---------------- Wizard ----------------

type wizardData struct {
	HostRAM   string
	Releases  []string
	Java      string
	Port      int
	PortRange string
	RAM       int
	MaxRAM    int
	Autopause bool
	HasCFKey  bool
}

func (a *App) maxRAM(ctx context.Context) int {
	h := a.Host.Read(ctx)
	if gbTotal := int(h.MemTotal >> 30); gbTotal > 2 {
		return min(gbTotal, 64)
	}
	return 16
}

func (a *App) releases(ctx context.Context) []string {
	rel, err := a.M.Sources.Releases(ctx)
	if err != nil || len(rel) == 0 {
		return []string{"1.21.11", "1.21.10", "1.21.8", "1.21.4", "1.21.1", "1.20.6", "1.20.4", "1.20.1", "1.19.4", "1.18.2", "1.16.5", "1.12.2"}
	}
	if len(rel) > 60 {
		rel = rel[:60]
	}
	return rel
}

func (a *App) wizardPage(w http.ResponseWriter, r *http.Request) {
	cfg := a.Cfg.Get()
	rel := a.releases(r.Context())
	d := wizardData{
		Releases: rel, Java: a.M.Sources.JavaTag(r.Context(), rel[0]), Port: a.M.NextPort(""),
		PortRange: fmt.Sprintf("%d–%d", cfg.PortMin, cfg.PortMax), RAM: cfg.DefaultMemoryGB, MaxRAM: a.maxRAM(r.Context()),
		Autopause: cfg.DefaultAutopause, HasCFKey: cfg.CFAPIKey != "", HostRAM: gb(a.Host.Read(r.Context()).MemTotal),
	}
	a.render(w, r, "wizard", Page{Title: "Neuer Server", Nav: "wizard", Data: d})
}

func (a *App) wizardJava(w http.ResponseWriter, r *http.Request) {
	a.partial(w, "wizard", "javaTag", a.M.Sources.JavaTag(r.Context(), r.FormValue("version")))
}

func (a *App) wizardPacks(w http.ResponseWriter, r *http.Request) {
	src := r.FormValue("src")
	q := strings.TrimSpace(r.FormValue("q_" + src))
	var packs []sources.Pack
	var err error
	switch src {
	case server.KindModrinth:
		packs, err = a.M.Sources.ModrinthSearch(r.Context(), q)
	case server.KindCurseForge:
		packs, err = a.M.Sources.CurseForgeSearch(r.Context(), q)
	default:
		err = errors.New("unbekannte Quelle")
	}
	msg := ""
	if errors.Is(err, sources.ErrUnauthorized) {
		msg = "CurseForge-API-Key fehlt oder ist ungültig. Hinterlege ihn in den Einstellungen."
	} else if err != nil {
		msg = "Suche fehlgeschlagen: " + err.Error()
	}
	a.partial(w, "wizard", "packs", map[string]any{"Packs": packs, "Error": msg})
}

func (a *App) packVersions(ctx context.Context, src, slug, id string) ([]sources.PackVersion, error) {
	if src == server.KindCurseForge {
		return a.M.Sources.CurseForgeVersions(ctx, id)
	}
	return a.M.Sources.ModrinthVersions(ctx, slug)
}

func (a *App) wizardPackVersions(w http.ResponseWriter, r *http.Request) {
	src, slug, id := r.FormValue("src"), r.FormValue("slug"), r.FormValue("id")
	vs, err := a.packVersions(r.Context(), src, slug, id)
	d := map[string]any{"Src": src, "ID": id, "Versions": vs, "Error": ""}
	if err != nil {
		d["Error"] = "Versionen konnten nicht geladen werden: " + err.Error()
	} else if len(vs) == 0 {
		d["Error"] = "Dieses Modpack hat keine Versionen."
	}
	// Name and icon come from the (cached) search result, not from the client.
	var packs []sources.Pack
	if src == server.KindCurseForge {
		packs, _ = a.M.Sources.CurseForgeSearch(r.Context(), strings.TrimSpace(r.FormValue("q_"+src)))
	} else {
		packs, _ = a.M.Sources.ModrinthSearch(r.Context(), strings.TrimSpace(r.FormValue("q_"+src)))
	}
	d["Name"], d["Icon"] = slug, ""
	for _, p := range packs {
		if p.Slug == slug {
			d["Name"], d["Icon"] = p.Title, p.IconURL
		}
	}
	a.partial(w, "wizard", "packVersions", d)
}

func formInt(r *http.Request, key string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

func checked(r *http.Request, key string) bool { return r.FormValue(key) == "on" }

// serverFromWizard builds a definition from the wizard form and validates it.
func (a *App) serverFromWizard(r *http.Request) (*server.Server, error) {
	ctx := r.Context()
	name := strings.TrimSpace(r.FormValue("name"))
	s := &server.Server{
		ID: server.Slug(name), Name: name, Kind: r.FormValue("kind"),
		Port: formInt(r, "port", 0, 1, 65535), MemoryGB: formInt(r, "memory", 4, 1, 64),
		Autopause: checked(r, "autopause"), Aikar: checked(r, "aikar"), Autostart: checked(r, "autostart"),
		Props: server.Props{
			MOTD: strings.TrimSpace(r.FormValue("motd")), Mode: r.FormValue("mode"), Difficulty: r.FormValue("difficulty"),
			MaxPlayers: formInt(r, "maxPlayers", 10, 1, 1000), Seed: strings.TrimSpace(r.FormValue("seed")),
			PVP: true, Whitelist: checked(r, "whitelist"), OnlineMode: checked(r, "onlineMode"),
		},
		CreatedAt: time.Now(),
	}
	if name == "" || s.ID == "" {
		return nil, errors.New("Bitte einen Namen mit Buchstaben oder Ziffern angeben.")
	}
	if a.M.Store.Exists(s.ID) {
		return nil, fmt.Errorf("Die ID „%s“ ist schon vergeben. Bitte einen anderen Namen wählen.", s.ID)
	}
	if s.Port == 0 {
		return nil, errors.New("Bitte einen gültigen Port angeben.")
	}
	if a.M.PortTaken(s.Port, "") {
		return nil, fmt.Errorf("Port %d nutzt schon ein anderer Server.", s.Port)
	}
	switch s.Kind {
	case server.KindSoftware:
		s.Type, s.Version = r.FormValue("type"), r.FormValue("version")
		valid := false
		for _, t := range server.Types {
			valid = valid || t.ID == s.Type
		}
		if !valid || s.Version == "" {
			return nil, errors.New("Bitte Server-Software und Version wählen.")
		}
	case server.KindModrinth, server.KindCurseForge:
		src := s.Kind
		slug, verID, id := r.FormValue("pack_slug_"+src), r.FormValue("pack_version_"+src), r.FormValue("pack_id_"+src)
		vs, err := a.packVersions(ctx, src, slug, id)
		if err != nil {
			return nil, fmt.Errorf("Modpack-Versionen nicht abrufbar: %w", err)
		}
		var pv *sources.PackVersion
		for i := range vs {
			if vs[i].ID == verID {
				pv = &vs[i]
			}
		}
		if slug == "" || pv == nil {
			return nil, errors.New("Bitte ein Modpack und eine Version wählen.")
		}
		s.Pack = &server.Pack{Slug: slug, ProjectID: id, Name: r.FormValue("pack_name_" + src), Version: pv.ID, VersionLabel: pv.Name,
			Loader: pv.Loader, MC: pv.MC, IconURL: r.FormValue("pack_icon_" + src)}
		if s.Pack.Name == "" {
			s.Pack.Name = slug
		}
		s.Version, s.Type = pv.MC, strings.ToUpper(pv.Loader)
	default:
		return nil, errors.New("Bitte eine Grundlage wählen.")
	}
	s.JavaTag = a.M.Sources.JavaTag(ctx, s.Version)
	return s, nil
}

func (a *App) wizardPreview(w http.ResponseWriter, r *http.Request) {
	s, err := a.serverFromWizard(r)
	if err != nil {
		a.partial(w, "wizard", "preview", map[string]any{"Error": err.Error()})
		return
	}
	compose := server.Compose(s, "")
	if a.Cfg.Get().CFAPIKey != "" && s.Kind == server.KindCurseForge {
		compose = server.Compose(s, "•••••• (aus den Einstellungen)")
	}
	a.partial(w, "wizard", "preview", map[string]any{"S": s, "Compose": compose, "Dir": a.M.Store.Dir(s.ID)})
}

func (a *App) createServer(w http.ResponseWriter, r *http.Request) {
	if !checked(r, "eula") {
		fail(w, "Ohne Zustimmung zur Minecraft-EULA kann kein Server starten.")
		return
	}
	s, err := a.serverFromWizard(r)
	if err == nil {
		err = a.M.Create(s)
	}
	if err != nil {
		fail(w, err.Error())
		return
	}
	redirect(w, r, "/servers/"+s.ID)
}

// ---------------- Server-Detail ----------------

type tab struct {
	Key, Label string
	V2         bool
}

var tabs = []tab{{"console", "Konsole", false}, {"settings", "Einstellungen", false}, {"players", "Spieler", false},
	{"mods", "Mods & Plugins", false}, {"backups", "Backups", false}, {"files", "Dateien", false}}

type serverPage struct {
	V          manager.View
	Tab        string
	Tabs       []tab
	Disk       int64
	Update     string
	Stats      statsData
	Players    playersData
	MaxRAM     int
	HostRAM    string
	CPUOptions []cpuOption
	CPUValue   string
	ExtraEnv   string
	FullEnv    string
	Bans       []string
	DataDir    string
	Backups    backupsData
	Addons     addonsData
}

type statsData struct {
	CPU     float64
	Spark   template.HTML
	MemUsed uint64
	MemGB   int
}

type playersData struct {
	ID     string
	Online []string
	Max    int
	Live   bool
}

func (a *App) loadServer(w http.ResponseWriter, r *http.Request) (*server.Server, bool) {
	s, err := a.M.Store.Get(r.PathValue("id"))
	if err != nil {
		if isHX(r) {
			fail(w, "Server nicht gefunden.")
		} else {
			http.NotFound(w, r)
		}
		return nil, false
	}
	return s, true
}

// updateHint checks Mojang/Modrinth/CurseForge (cached) for something newer.
func (a *App) updateHint(ctx context.Context, s *server.Server) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.Pack != nil {
		vs, err := a.packVersions(ctx, s.Kind, s.Pack.Slug, s.Pack.ProjectID)
		if err == nil && len(vs) > 0 && vs[0].ID != s.Pack.Version {
			return fmt.Sprintf("%s %s → %s", s.Pack.Name, s.Pack.VersionLabel, vs[0].Name)
		}
		return ""
	}
	rel, err := a.M.Sources.Releases(ctx)
	if err == nil && len(rel) > 0 && sources.IsVersion(s.Version) && sources.CompareMC(rel[0], s.Version) > 0 {
		return fmt.Sprintf("Minecraft %s → %s", s.Version, rel[0])
	}
	return ""
}

func (a *App) serverPageData(ctx context.Context, s *server.Server, tabKey string) serverPage {
	d := serverPage{V: a.M.View(ctx, s, true), Tab: tabKey, Tabs: tabs, Disk: a.M.DiskUsage(s), MaxRAM: a.maxRAM(ctx), HostRAM: gb(a.Host.Read(ctx).MemTotal), DataDir: a.M.Store.DataDir(s)}
	if !d.V.Status.Busy() {
		d.Update = a.updateHint(ctx, s)
	}
	d.Stats = statsData{Spark: spark(nil), MemUsed: d.V.MemUsed, MemGB: s.MemoryGB}
	d.Players = playersData{ID: s.ID, Online: d.V.Online, Max: d.V.MaxPlayers, Live: d.V.Status.Live()}
	switch tabKey {
	case "settings":
		d.CPUOptions, d.CPUValue = a.cpuOptions(ctx, s)
		d.ExtraEnv = server.FormatEnv(s.ExtraEnv)
		env := s.Env(a.Cfg.Get().CFAPIKey)
		for i := range env {
			if env[i].Key == "CF_API_KEY" {
				env[i].Value = "••••••"
			}
		}
		d.FullEnv = server.FormatEnv(env)
	case "players":
		d.Bans = a.bans(s)
	case "backups":
		d.Backups = a.backupsData(s, d.V.Status)
	}
	return d
}

func (a *App) serverTabData(r *http.Request, d *serverPage, s *server.Server) {
	if d.Tab == "mods" {
		d.Addons = a.addonsData(r, s)
	}
}

type cpuOption struct{ Value, Label string }

func cpuValue(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (a *App) cpuOptions(ctx context.Context, s *server.Server) ([]cpuOption, string) {
	cores := a.Host.Read(ctx).Cores
	if cores <= 0 {
		cores = 8
	}
	opts := []cpuOption{{"0", "Unbegrenzt"}}
	for _, n := range []float64{0.5, 1, 2, 3, 4, 6, 8, 12, 16} {
		if n <= float64(cores) {
			label := cpuValue(n) + " Kerne"
			if n == 1 {
				label = "1 Kern"
			} else if n == 0.5 {
				label = "½ Kern"
			}
			opts = append(opts, cpuOption{cpuValue(n), label})
		}
	}
	cur := cpuValue(s.CPUs)
	found := false
	for _, o := range opts {
		found = found || o.Value == cur
	}
	if !found {
		opts = append(opts, cpuOption{cur, strings.Replace(cur, ".", ",", 1) + " Kerne"})
	}
	return opts, cur
}

func (a *App) bans(s *server.Server) []string {
	b, err := a.M.ReadDataFile(s, "banned-players.json")
	if err != nil {
		return nil
	}
	var list []struct{ Name string }
	_ = json.Unmarshal(b, &list)
	var out []string
	for _, e := range list {
		out = append(out, e.Name)
	}
	return server.NormalizeNames(out)
}

func (a *App) serverPage(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	t := r.FormValue("tab")
	valid := false
	for _, x := range tabs {
		valid = valid || x.Key == t
	}
	if !valid {
		t = "console"
	}
	d := a.serverPageData(r.Context(), s, t)
	a.serverTabData(r, &d, s)
	a.render(w, r, "server", Page{Title: s.Name, Nav: "server", Current: s.ID, Data: d})
}

func (a *App) serverAction(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	var err error
	msg := ""
	switch r.PathValue("action") {
	case "start":
		err, msg = a.M.Start(s.ID), s.Name+" wird gestartet."
	case "stop":
		err, msg = a.M.Stop(s.ID), s.Name+" wird gestoppt. Die Welt wird gespeichert."
	case "restart":
		err, msg = a.M.Restart(s.ID), s.Name+" wird neu gestartet."
	default:
		err = errors.New("unbekannte Aktion")
	}
	if err != nil {
		fail(w, err.Error())
		return
	}
	toast(w, msg, "")
	time.Sleep(150 * time.Millisecond) // let the op register before rendering
	v := a.M.View(r.Context(), s, false)
	if r.FormValue("ctx") == "row" {
		a.partial(w, "overview", "serverRow", v)
		return
	}
	a.partial(w, "server", "srvHead", serverPage{V: v, Disk: a.M.DiskUsage(s)})
}

func (a *App) serverCommand(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.FormValue("cmd")), "/"))
	if cmd == "" || len(cmd) > 400 || strings.ContainsAny(cmd, "\n\r") {
		fail(w, "Ungültiger Befehl.")
		return
	}
	switch st := a.M.View(r.Context(), s, false).Status; {
	case st == server.StatusPaused:
		fail(w, "Der Server ist pausiert. Er wacht auf, sobald sich ein Spieler verbindet; RCON-Befehle wecken ihn nicht.")
		return
	case !st.Live() && st != server.StatusStarting:
		fail(w, "Der Server läuft nicht.")
		return
	}
	var b strings.Builder
	line := func(level, text string) {
		out, _ := a.renderString("server", "logLine", map[string]string{"Level": level, "Text": text})
		b.WriteString(out)
	}
	line("cmd", "> "+cmd)
	out, err := a.M.RCON(r.Context(), s, cmd)
	a.M.Forget(s.ID)
	switch {
	case err != nil:
		line("error", "RCON: "+err.Error())
	case out == "":
		line("info", "(keine Ausgabe)")
	default:
		for _, l := range strings.Split(out, "\n") {
			line(server.Level(l), l)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, b.String())
}

func (a *App) serverSettings(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	extra, err := server.ParseEnv(r.FormValue("extraEnv"))
	if err != nil {
		fail(w, "Zusätzliche Variablen: "+err.Error())
		return
	}
	port := formInt(r, "port", s.Port, 1, 65535)
	if a.M.PortTaken(port, s.ID) {
		fail(w, fmt.Sprintf("Port %d nutzt schon ein anderer Server.", port))
		return
	}
	s.Port = port
	s.MemoryGB = formInt(r, "memory", s.MemoryGB, 1, 64)
	s.Aikar, s.Autostart = checked(r, "aikar"), checked(r, "autostart")
	switch r.FormValue("idle") {
	case "stop":
		s.Autostop, s.Autopause = true, false
	case "none":
		s.Autostop, s.Autopause = false, false
	default:
		s.Autostop, s.Autopause = false, true
	}
	s.AutostopMinutes = formInt(r, "idleMinutes", 30, 5, 1440)
	if cpus, err := strconv.ParseFloat(r.FormValue("cpus"), 64); err == nil && cpus >= 0 && cpus <= 256 {
		s.CPUs = cpus
	}
	s.Props.MOTD = strings.TrimSpace(r.FormValue("motd"))
	s.Props.Mode, s.Props.Difficulty = r.FormValue("mode"), r.FormValue("difficulty")
	s.Props.MaxPlayers = formInt(r, "maxPlayers", s.Props.MaxPlayers, 1, 1000)
	s.Props.ViewDistance = formInt(r, "viewDistance", 0, 0, 32)
	s.Props.SimDistance = formInt(r, "simDistance", 0, 0, 32)
	s.Props.Seed = strings.TrimSpace(r.FormValue("seed"))
	s.Props.PVP, s.Props.Whitelist, s.Props.OnlineMode = checked(r, "pvp"), checked(r, "whitelist"), checked(r, "onlineMode")
	s.ExtraEnv = extra
	restart := r.FormValue("apply") == "1"
	if err := a.M.Apply(s, false, restart); err != nil {
		fail(w, err.Error())
		return
	}
	if restart {
		redirect(w, r, "/servers/"+s.ID+"?tab=console")
		return
	}
	toast(w, "Gespeichert. Greift beim nächsten Neustart.", "ok")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) serverPlayers(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	list, op := r.PathValue("list"), r.PathValue("op")
	name := strings.TrimSpace(r.FormValue("player"))
	if !server.ValidPlayerName(name) {
		fail(w, "Spielernamen bestehen aus 2–16 Buchstaben, Ziffern oder Unterstrichen.")
		return
	}
	v := a.M.View(r.Context(), s, false)
	cmds := map[string][2]string{"whitelist": {"whitelist add ", "whitelist remove "}, "ops": {"op ", "deop "}, "bans": {"ban ", "pardon "}}
	c, ok := cmds[list]
	if !ok || (op != "add" && op != "remove") {
		fail(w, "Unbekannte Liste.")
		return
	}
	edit := func(in []string) []string {
		if op == "add" {
			return server.NormalizeNames(append(in, name))
		}
		var out []string
		for _, n := range in {
			if !strings.EqualFold(n, name) {
				out = append(out, n)
			}
		}
		return out
	}
	switch list {
	case "whitelist":
		s.Whitelist = edit(s.Whitelist)
	case "ops":
		s.Ops = edit(s.Ops)
	}
	if list != "bans" {
		if err := a.M.Apply(s, false, false); err != nil {
			fail(w, err.Error())
			return
		}
	}
	msg := name + map[string]string{"add": " hinzugefügt.", "remove": " entfernt."}[op]
	if v.Status.Live() {
		cmd := c[0]
		if op == "remove" {
			cmd = c[1]
		}
		if _, err := a.M.RCON(r.Context(), s, cmd+name); err != nil {
			msg += " RCON: " + err.Error()
		} else if list == "bans" {
			time.Sleep(300 * time.Millisecond) // banned-players.json is written asynchronously
		}
	} else if list == "bans" {
		fail(w, "Bans gehen nur, während der Server läuft.")
		return
	}
	a.M.Forget(s.ID)
	toast(w, msg, "ok")
	d := serverPage{V: v, Bans: a.bans(s)}
	d.V.S = s
	a.partial(w, "server", "playersBody", d)
}

// ---------------- Update & Deinstallation ----------------

type updateData struct {
	S            *server.Server
	Mode         string
	Modes        []tab
	Releases     []string
	Target       string
	PackVersions []sources.PackVersion
	Error        string
}

func (a *App) updateModal(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	d := updateData{S: s, Mode: r.FormValue("mode")}
	if s.Pack != nil {
		d.Modes = []tab{{Key: "pack", Label: "Modpack-Version"}, {Key: "image", Label: "Server-Image"}}
	} else {
		d.Modes = []tab{{Key: "mc", Label: "Minecraft-Version"}, {Key: "image", Label: "Server-Image"}}
	}
	if d.Mode != "image" {
		d.Mode = d.Modes[0].Key
	}
	switch d.Mode {
	case "mc":
		d.Releases = a.releases(r.Context())
		d.Target = s.Version
		if len(d.Releases) > 0 && (!sources.IsVersion(s.Version) || sources.CompareMC(d.Releases[0], s.Version) > 0) {
			d.Target = d.Releases[0]
		}
	case "pack":
		vs, err := a.packVersions(r.Context(), s.Kind, s.Pack.Slug, s.Pack.ProjectID)
		if err != nil {
			d.Error = "Versionen konnten nicht geladen werden: " + err.Error()
		}
		d.PackVersions = vs
	}
	a.partial(w, "server", "updateModal", d)
}

func (a *App) updatePost(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	msg := ""
	switch r.FormValue("mode") {
	case "mc":
		v, t := r.FormValue("version"), r.FormValue("type")
		valid := false
		for _, x := range server.Types {
			valid = valid || x.ID == t
		}
		if !valid || v == "" {
			fail(w, "Bitte Version und Software wählen.")
			return
		}
		if sources.IsVersion(s.Version) && sources.CompareMC(v, s.Version) < 0 && !checked(r, "allowDowngrade") {
			fail(w, "Downgrade von "+s.Version+" auf "+v+" nur mit ausdrücklicher Bestätigung.")
			return
		}
		s.Version, s.Type = v, t
		s.JavaTag = a.M.Sources.JavaTag(ctx, v)
		msg = fmt.Sprintf("Update auf %s %s gestartet.", server.TypeName(t), v)
	case "pack":
		if s.Pack == nil {
			fail(w, "Kein Modpack-Server.")
			return
		}
		vs, err := a.packVersions(ctx, s.Kind, s.Pack.Slug, s.Pack.ProjectID)
		if err != nil {
			fail(w, err.Error())
			return
		}
		var pv *sources.PackVersion
		for i := range vs {
			if vs[i].ID == r.FormValue("packVersion") {
				pv = &vs[i]
			}
		}
		if pv == nil {
			fail(w, "Version nicht gefunden.")
			return
		}
		s.Pack.Version, s.Pack.VersionLabel, s.Pack.MC = pv.ID, pv.Name, pv.MC
		if pv.Loader != "" {
			s.Pack.Loader, s.Type = pv.Loader, strings.ToUpper(pv.Loader)
		}
		if pv.MC != "" {
			s.Version = pv.MC
			s.JavaTag = a.M.Sources.JavaTag(ctx, pv.MC)
		}
		msg = fmt.Sprintf("Update auf %s %s gestartet.", s.Pack.Name, pv.Name)
	default:
		msg = "Image wird aktualisiert."
	}
	if err := a.M.ApplyUpdate(s, true, true, checked(r, "backupFirst")); err != nil {
		fail(w, err.Error())
		return
	}
	toast(w, msg, "", "fc-close-modal")
	redirect(w, r, "/servers/"+s.ID+"?tab=console")
}

func (a *App) uninstallModal(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	a.partial(w, "server", "uninstallModal", map[string]any{"S": s, "Dir": a.M.Store.Dir(s.ID), "Archive": a.M.ArchiveDir()})
}

func (a *App) uninstallPost(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	if r.FormValue("confirm") != s.ID {
		fail(w, "Bestätigung stimmt nicht.")
		return
	}
	mode := manager.RemoveKeepData
	switch r.FormValue("mode") {
	case "all":
		mode = manager.RemoveAll
	case "backup":
		mode = manager.RemoveBackup
	}
	if err := a.M.Uninstall(s.ID, mode); err != nil {
		fail(w, err.Error())
		return
	}
	redirect(w, r, "/")
}

// ---------------- Einstellungen ----------------

type settingsData struct {
	Cfg        config.Settings
	RAMOptions []int
	DataDir    string
	Listen     string
	Docker     docker.Info
	DockerErr  string
	DockerHost string
	Version    string
	SelfMB     uint64
	Started    time.Time
}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	d := settingsData{Cfg: a.Cfg.Get(), RAMOptions: []int{2, 3, 4, 6, 8, 10, 12, 16}, DataDir: a.DataDir, Listen: a.Listen,
		DockerHost: a.M.Docker.Host, Version: Version, SelfMB: selfMB(), Started: a.start}
	info, err := a.M.Docker.Info(r.Context())
	if err != nil {
		d.DockerErr = err.Error()
	}
	d.Docker = info
	a.render(w, r, "settings", Page{Title: "Einstellungen", Nav: "settings", Data: d})
}

func (a *App) settingsPost(w http.ResponseWriter, r *http.Request) {
	pmin, pmax := formInt(r, "portMin", 25565, 1024, 65535), formInt(r, "portMax", 25600, 1024, 65535)
	if pmax < pmin {
		fail(w, "Der Port-Bereich ist ungültig.")
		return
	}
	err := a.Cfg.Update(func(s *config.Settings) {
		s.CFAPIKey = strings.TrimSpace(r.FormValue("cfApiKey"))
		s.PortMin, s.PortMax = pmin, pmax
		s.DefaultMemoryGB = formInt(r, "defaultMemoryGb", 4, 1, 64)
		s.SessionDays = formInt(r, "sessionDays", 7, 1, 30)
		s.DefaultAutopause = checked(r, "defaultAutopause")
	})
	if err != nil {
		fail(w, err.Error())
		return
	}
	toast(w, "Einstellungen gespeichert.", "ok")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) passwordPost(w http.ResponseWriter, r *http.Request) {
	if !auth.CheckPassword(a.Cfg.Get().PasswordHash, r.FormValue("current")) {
		a.lim.Fail(r)
		fail(w, "Das aktuelle Passwort ist falsch.")
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 || pw != r.FormValue("password2") {
		fail(w, "Neues Passwort: mindestens 10 Zeichen und beide Felder gleich.")
		return
	}
	hash, err := auth.HashPassword(pw)
	if err == nil {
		err = a.Cfg.Update(func(s *config.Settings) { s.PasswordHash = hash })
	}
	if err != nil {
		fail(w, err.Error())
		return
	}
	a.sess.Issue(w) // the old cookie is now invalid
	toast(w, "Passwort geändert. Andere Sitzungen sind abgemeldet.", "ok")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) cfCheck(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.FormValue("cfApiKey"))
	if key == "" {
		fmt.Fprint(w, `<span style="color:var(--crit)">Kein Key eingetragen.</span>`)
		return
	}
	if err := a.M.Sources.CurseForgeCheck(r.Context(), key); err != nil {
		fmt.Fprintf(w, `<span style="color:var(--crit)">%s</span>`, template.HTMLEscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, `<span style="color:var(--ok)">Gültig. Speichern nicht vergessen.</span>`)
}
