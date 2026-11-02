package agent

import (
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"cubeherd/internal/api"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
)

// Upload limits mirror the dashboard's own.
const (
	maxFileBody  = 256 << 20
	maxWorldBody = 16 << 30
)

func (a *Agent) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/paths", a.paths)
	mux.HandleFunc("GET /v1/servers/{id}/files", a.list)
	mux.HandleFunc("GET /v1/servers/{id}/files/stat", a.stat)
	mux.HandleFunc("GET /v1/servers/{id}/files/text", a.text)
	mux.HandleFunc("GET /v1/servers/{id}/files/content", a.content)
	mux.HandleFunc("PUT /v1/servers/{id}/files/content", a.writeContent)
	mux.HandleFunc("DELETE /v1/servers/{id}/files", a.remove)
	mux.HandleFunc("GET /v1/servers/{id}/data", a.dataFile)
	mux.HandleFunc("GET /v1/servers/{id}/world.zip", a.worldZip)
	mux.HandleFunc("PUT /v1/servers/{id}/world/stage", a.stageWorld)
	mux.HandleFunc("POST /v1/servers/{id}/world/import", a.importWorld)

	mux.HandleFunc("GET /v1/servers/{id}/addons", a.addonFiles)
	mux.HandleFunc("GET /v1/servers/{id}/addons/pending", a.addonsPending)
	mux.HandleFunc("PUT /v1/servers/{id}/addons/{name}", a.uploadAddon)
	mux.HandleFunc("DELETE /v1/servers/{id}/addons/{name}", a.deleteAddon)
}

// lazyWriter sends the status line only with the first byte, so an error before any data is still a clean error response.
type lazyWriter struct {
	w       http.ResponseWriter
	ctype   string
	started bool
}

func (l *lazyWriter) start() {
	if !l.started {
		l.started = true
		l.w.Header().Set("Content-Type", l.ctype)
		l.w.WriteHeader(http.StatusOK)
	}
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	l.start()
	return l.w.Write(p)
}

// streamTo runs fn against the response body. A failure before the first byte is answered as an error;
// after that the connection is aborted so the dashboard sees a truncated download instead of a short, "successful" one.
func streamTo(w http.ResponseWriter, ctype string, fn func(io.Writer) error) {
	lw := &lazyWriter{w: w, ctype: ctype}
	if err := fn(lw); err != nil {
		if !lw.started {
			fail(w, err)
			return
		}
		panic(http.ErrAbortHandler)
	}
	lw.start()
}

// rel reads the ?path= parameter as a clean path below /data.
func rel(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, err := manager.CleanRel(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return "", false
	}
	return p, true
}

func (a *Agent) paths(w http.ResponseWriter, r *http.Request) {
	var s server.Server
	if decode(w, r, &s) {
		writeJSON(w, http.StatusOK, a.B.Paths(&s))
	}
}

func (a *Agent) list(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, ok := rel(w, r)
	if !ok {
		return
	}
	entries, truncated, err := a.B.List(r.Context(), s, p)
	if err != nil {
		fail(w, err)
		return
	}
	if entries == nil {
		entries = []api.Entry{}
	}
	writeJSON(w, http.StatusOK, api.Listing{Entries: entries, Truncated: truncated})
}

func (a *Agent) stat(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, ok := rel(w, r)
	if !ok {
		return
	}
	e, err := a.B.Stat(r.Context(), s, p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *Agent) text(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, ok := rel(w, r)
	if !ok {
		return
	}
	txt, isText, err := a.B.ReadText(r.Context(), s, p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.Text{Content: txt, IsText: isText})
}

func (a *Agent) content(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, ok := rel(w, r)
	if !ok {
		return
	}
	streamTo(w, "application/octet-stream", func(out io.Writer) error { return a.B.Open(r.Context(), s, p, out) })
}

func (a *Agent) writeContent(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, ok := rel(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBody)
	if err := a.B.WriteFile(r.Context(), s, p, r.Body); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) remove(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, ok := rel(w, r)
	if !ok {
		return
	}
	if err := a.B.Remove(r.Context(), s, p); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) dataFile(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	b, err := a.B.ReadDataFile(s, r.URL.Query().Get("name"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}

func (a *Agent) worldZip(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	streamTo(w, "application/zip", func(out io.Writer) error { return a.B.WorldZip(r.Context(), s, out) })
}

func (a *Agent) stageWorld(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorldBody)
	path, err := a.B.StageWorld(s, r.Body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"Path": path})
}

// importWorld imports the ZIP staged for this server. The dashboard never names a path, so it cannot
// make the agent unpack an arbitrary file.
func (a *Agent) importWorld(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	staged := filepath.Join(a.B.Paths(s).Dir, ".world-upload.zip")
	if _, err := os.Stat(staged); err != nil {
		fail(w, fs.ErrNotExist)
		return
	}
	if err := a.B.ImportWorld(s, staged); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Agent) addonFiles(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	files, err := a.B.AddonFiles(r.Context(), s)
	if err != nil {
		fail(w, err)
		return
	}
	if files == nil {
		files = []api.AddonFile{}
	}
	writeJSON(w, http.StatusOK, files)
}

func (a *Agent) addonsPending(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]bool{"Pending": a.B.AddonsPending(r.Context(), s)})
	}
}

// uploadAddon and deleteAddon answer with the saved definition: the list of uploaded files lives in it.
func (a *Agent) uploadAddon(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	name, _ := url.PathUnescape(r.PathValue("name"))
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBody)
	if err := a.B.UploadAddon(r.Context(), s, name, r.Body); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *Agent) deleteAddon(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	name, _ := url.PathUnescape(r.PathValue("name"))
	if err := a.B.DeleteUpload(r.Context(), s, name); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}
