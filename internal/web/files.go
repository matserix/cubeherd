package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"forgecraft/internal/manager"
	"forgecraft/internal/server"
)

type crumb struct{ Name, Path string }

type treeData struct {
	ID        string
	Path      string
	Crumbs    []crumb
	Entries   []manager.Entry
	Truncated bool
	Error     string
}

func crumbs(rel string) []crumb {
	out := []crumb{{Name: "data", Path: ""}}
	if rel == "" {
		return out
	}
	acc := ""
	for _, p := range strings.Split(rel, "/") {
		acc = path.Join(acc, p)
		out = append(out, crumb{Name: p, Path: acc})
	}
	return out
}

func joinRel(dir, name string) string { return path.Join(dir, name) }

func (a *App) relParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	rel, err := manager.CleanRel(r.FormValue("path"))
	if err != nil {
		fail(w, err.Error())
		return "", false
	}
	return rel, true
}

func (a *App) fileTree(w http.ResponseWriter, r *http.Request, s *server.Server, rel string) {
	d := treeData{ID: s.ID, Path: rel, Crumbs: crumbs(rel)}
	var err error
	d.Entries, d.Truncated, err = a.M.List(r.Context(), s, rel)
	if err != nil {
		d.Error = "Ordner nicht lesbar: " + err.Error()
	}
	a.partial(w, "server", "fileTree", d)
}

func (a *App) filesList(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	rel, ok := a.relParam(w, r)
	if !ok {
		return
	}
	a.fileTree(w, r, s, rel)
}

var fileHints = map[string]string{
	"server.properties": "itzg schreibt beim Start die Werte aus den Forgecraft-Einstellungen hinein. Änderungen an diesen Schlüsseln werden überschrieben; alles andere bleibt.",
	"whitelist.json":    "Wird beim Start mit der Liste im Reiter „Spieler“ abgeglichen. Bitte dort pflegen.",
	"ops.json":          "Wird beim Start mit der Liste im Reiter „Spieler“ abgeglichen. Bitte dort pflegen.",
	"eula.txt":          "Wird von itzg verwaltet.",
}

func (a *App) filesView(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	rel, ok := a.relParam(w, r)
	if !ok {
		return
	}
	st, err := a.M.Stat(r.Context(), s, rel)
	if err != nil || st.Dir {
		fail(w, "Datei nicht gefunden.")
		return
	}
	text, isText, err := a.M.ReadText(r.Context(), s, rel)
	if err != nil {
		fail(w, "Datei nicht lesbar: "+err.Error())
		return
	}
	a.partial(w, "server", "fileEditor", map[string]any{"ID": s.ID, "Path": rel, "Entry": st, "Text": text, "IsText": isText,
		"Hint": fileHints[rel], "Live": a.M.View(r.Context(), s, false).Status.Up()})
}

func (a *App) filesSave(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	rel, ok := a.relParam(w, r)
	if !ok || rel == "" {
		return
	}
	content := strings.ReplaceAll(r.FormValue("content"), "\r\n", "\n")
	if len(content) > manager.MaxEditSize {
		fail(w, "Die Datei ist zu groß für den Editor.")
		return
	}
	if err := a.M.WriteFile(r.Context(), s, rel, strings.NewReader(content)); err != nil {
		fail(w, "Speichern fehlgeschlagen: "+err.Error())
		return
	}
	toast(w, path.Base(rel)+" gespeichert.", "ok")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) filesUpload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	dir, err := manager.CleanRel(r.FormValue("path"))
	if err != nil {
		fail(w, err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, "Upload fehlgeschlagen (maximal 256 MB): "+err.Error())
		return
	}
	defer f.Close()
	name := filepath.Base(strings.ReplaceAll(hdr.Filename, "\\", "/"))
	if name == "." || name == ".." || name == "/" || name == "" {
		fail(w, "Ungültiger Dateiname.")
		return
	}
	if err := a.M.WriteFile(r.Context(), s, joinRel(dir, name), f); err != nil {
		fail(w, "Upload fehlgeschlagen: "+err.Error())
		return
	}
	toast(w, name+" hochgeladen.", "ok")
	a.fileTree(w, r, s, dir)
}

func (a *App) filesDelete(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	rel, ok := a.relParam(w, r)
	if !ok {
		return
	}
	if rel == "" {
		fail(w, "/data selbst lässt sich nicht löschen.")
		return
	}
	if err := a.M.Remove(r.Context(), s, rel); err != nil {
		fail(w, "Löschen fehlgeschlagen: "+err.Error())
		return
	}
	toast(w, path.Base(rel)+" gelöscht.", "ok")
	w.Header().Set("HX-Retarget", "#file-tree")
	w.Header().Set("HX-Reswap", "innerHTML")
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	a.fileTree(w, r, s, parent)
}

func attachment(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
}

func (a *App) filesDownload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	rel, err := manager.CleanRel(r.FormValue("path"))
	if err != nil || rel == "" {
		http.NotFound(w, r)
		return
	}
	st, err := a.M.Stat(r.Context(), s, rel)
	if err != nil || st.Dir {
		http.NotFound(w, r)
		return
	}
	attachment(w, path.Base(rel))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(st.Size))
	_ = a.M.Open(r.Context(), s, rel, w)
}

func (a *App) worldDownload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	attachment(w, fmt.Sprintf("%s-%s.zip", s.ID, time.Now().Format("20060102-1504")))
	w.Header().Set("Content-Type", "application/zip")
	if err := a.M.WorldZip(r.Context(), s, w); err != nil {
		// Headers may already be sent; the browser then shows a failed download.
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

const maxWorldUpload = 16 << 30

func (a *App) worldUpload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorldUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, "Upload fehlgeschlagen: "+err.Error())
		return
	}
	// Stream the ZIP to disk next to the server (not into /data) without buffering it in memory.
	tmp := filepath.Join(a.M.Store.Dir(s.ID), ".world-upload.zip")
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			fail(w, "Keine Datei empfangen.")
			return
		}
		if err != nil {
			fail(w, "Upload fehlgeschlagen: "+err.Error())
			return
		}
		if part.FormName() != "world" {
			continue
		}
		f, err := os.Create(tmp)
		if err != nil {
			fail(w, err.Error())
			return
		}
		_, err = io.Copy(f, part)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			fail(w, "Upload abgebrochen: "+err.Error())
			return
		}
		break
	}
	if err := a.M.ImportWorld(s, tmp); err != nil {
		fail(w, err.Error())
		return
	}
	redirect(w, r, "/servers/"+s.ID+"?tab=console")
}
