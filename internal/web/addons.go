package web

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"forgecraft/internal/manager"
	"forgecraft/internal/server"
	"forgecraft/internal/sources"
)

type addonsData struct {
	S        *server.Server
	Word     string // "Mods" or "Plugins"
	Files    []manager.AddonFile
	Others   int // files that belong to itzg/the modpack
	Pending  bool
	Up       bool
	Sources  []tab
	HasCFKey bool
}

func addonWord(s *server.Server) string {
	if server.IsPluginType(s.Type) {
		return "Plugins"
	}
	return "Mods"
}

func (a *App) addonsData(r *http.Request, s *server.Server) addonsData {
	d := addonsData{S: s, Word: addonWord(s), HasCFKey: a.Cfg.Get().CFAPIKey != ""}
	d.Files, _ = a.M.AddonFiles(r.Context(), s)
	for _, f := range d.Files {
		if !f.Uploaded {
			d.Others++
		}
	}
	d.Up = a.M.View(r.Context(), s, false).Status.Up()
	d.Pending = d.Up && a.M.AddonsPending(r.Context(), s)
	d.Sources = []tab{{Key: server.AddonModrinth, Label: "Modrinth"}, {Key: server.AddonCurseForge, Label: "CurseForge"}}
	if server.IsPluginType(s.Type) {
		d.Sources = append(d.Sources, tab{Key: server.AddonSpiget, Label: "Spigot"})
	}
	return d
}

func addonQuery(s *server.Server, text string) sources.AddonQuery {
	return sources.AddonQuery{Text: text, Plugins: server.IsPluginType(s.Type), Loader: strings.ToLower(s.Type), MC: s.Version}
}

func (a *App) addonSearch(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	q := addonQuery(s, strings.TrimSpace(r.FormValue("q")))
	var hits []sources.AddonHit
	var err error
	src := r.FormValue("src")
	switch src {
	case server.AddonCurseForge:
		hits, err = a.M.Sources.CurseForgeAddons(r.Context(), q)
	case server.AddonSpiget:
		hits, err = a.M.Sources.SpigetAddons(r.Context(), q)
	default:
		src = server.AddonModrinth
		hits, err = a.M.Sources.ModrinthAddons(r.Context(), q)
	}
	msg := ""
	switch {
	case errors.Is(err, sources.ErrUnauthorized):
		msg = "Für die CurseForge-Suche brauchst du einen API-Key in den Einstellungen."
	case err != nil && src == server.AddonSpiget:
		msg = "Spigot antwortet gerade nicht. Viele Plugins gibt es auch bei Modrinth."
	case err != nil:
		msg = "Suche fehlgeschlagen: " + err.Error()
	}
	installed := map[string]bool{}
	for _, ad := range s.Addons {
		installed[ad.Source+":"+ad.Ref] = true
	}
	a.partial(w, "server", "addonResults", map[string]any{"S": s, "Hits": hits, "Error": msg, "Installed": installed})
}

func (a *App) addonAdd(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	ad := server.Addon{Source: r.FormValue("src"), Ref: strings.TrimSpace(r.FormValue("ref")), Name: strings.TrimSpace(r.FormValue("name")), IconURL: r.FormValue("icon")}
	if !server.ValidAddon(ad) {
		fail(w, "Ungültiger Eintrag.")
		return
	}
	if !strings.HasPrefix(ad.IconURL, "https://") {
		ad.IconURL = ""
	}
	if ad.Name == "" || len(ad.Name) > 80 {
		ad.Name = ad.Ref
	}
	for _, x := range s.Addons {
		if x.Source == ad.Source && x.Ref == ad.Ref {
			fail(w, ad.Name+" ist schon eingetragen.")
			return
		}
	}
	s.AddAddon(ad)
	if err := a.M.Apply(s, false, false); err != nil {
		fail(w, err.Error())
		return
	}
	toast(w, ad.Name+" wird beim nächsten Start installiert.", "ok")
	a.partial(w, "server", "addonInstalled", a.addonsData(r, s))
}

func (a *App) addonRemove(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	src, ref := r.FormValue("src"), r.FormValue("ref")
	var rest []server.Addon
	name := ""
	for _, x := range s.Addons {
		if x.Source == src && x.Ref == ref {
			name = x.Name
			continue
		}
		rest = append(rest, x)
	}
	if name == "" {
		fail(w, "Eintrag nicht gefunden.")
		return
	}
	s.Addons = rest
	if err := a.M.Apply(s, false, false); err != nil {
		fail(w, err.Error())
		return
	}
	msg := name + " wird beim nächsten Start entfernt."
	if src == server.AddonSpiget {
		msg = name + " ist ausgetragen. Spigot-Plugins entfernt itzg nicht selbst; die JAR bleibt im plugins-Ordner."
	}
	toast(w, msg, "ok")
	a.partial(w, "server", "addonInstalled", a.addonsData(r, s))
}

const maxUpload = 256 << 20

func (a *App) addonUpload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	f, hdr, err := r.FormFile("jar")
	if err != nil {
		fail(w, "Upload fehlgeschlagen (maximal 256 MB): "+err.Error())
		return
	}
	defer f.Close()
	name := filepath.Base(hdr.Filename)
	if err := a.M.UploadAddon(r.Context(), s, name, f); err != nil {
		fail(w, err.Error())
		return
	}
	toast(w, name+" hochgeladen. Wird beim nächsten Start geladen.", "ok")
	a.partial(w, "server", "addonInstalled", a.addonsData(r, s))
}

func (a *App) addonDeleteUpload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	name := r.FormValue("file")
	if err := a.M.DeleteUpload(r.Context(), s, name); err != nil {
		fail(w, err.Error())
		return
	}
	toast(w, name+" gelöscht.", "ok")
	a.partial(w, "server", "addonInstalled", a.addonsData(r, s))
}
