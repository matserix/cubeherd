package agent

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strconv"

	"cubeherd/internal/api"
	"cubeherd/internal/docker"
	"cubeherd/internal/server"
)

const maxBody = 4 << 20 // definitions and commands are small

func (a *Agent) routes(mux *http.ServeMux) {
	a.fileRoutes(mux)
	a.backupRoutes(mux)
	mux.HandleFunc("GET /v1/docker", a.dockerInfo)
	mux.HandleFunc("GET /v1/servers", a.servers)
	mux.HandleFunc("POST /v1/servers", a.create)
	mux.HandleFunc("GET /v1/servers/{id}", a.get)
	mux.HandleFunc("PUT /v1/servers/{id}", a.apply)
	mux.HandleFunc("DELETE /v1/servers/{id}", a.uninstall)
	mux.HandleFunc("GET /v1/servers/{id}/view", a.view)
	mux.HandleFunc("GET /v1/servers/{id}/busy", a.busy)
	mux.HandleFunc("GET /v1/servers/{id}/disk", a.disk)
	mux.HandleFunc("POST /v1/servers/{id}/update", a.update)
	mux.HandleFunc("POST /v1/servers/{id}/{action}", a.lifecycle)
	mux.HandleFunc("GET /v1/ports/next", a.nextPort)
	mux.HandleFunc("GET /v1/ports/taken", a.portTaken)
	mux.HandleFunc("GET /v1/discover", a.discover)
	mux.HandleFunc("POST /v1/import", a.importContainer)

	mux.HandleFunc("POST /v1/servers/{id}/rcon", a.rcon)
	mux.HandleFunc("GET /v1/servers/{id}/players", a.players)
	mux.HandleFunc("GET /v1/servers/{id}/perf", a.perf)

	mux.HandleFunc("GET /v1/servers/{id}/container", a.container)
	mux.HandleFunc("GET /v1/servers/{id}/stats", a.stats)
	mux.HandleFunc("GET /v1/servers/{id}/logs", a.logs)
	mux.HandleFunc("GET /v1/servers/{id}/ops", a.ops)
}

func (a *Agent) dockerInfo(w http.ResponseWriter, r *http.Request) {
	in, err := a.B.DockerInfo(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (a *Agent) servers(w http.ResponseWriter, r *http.Request) {
	views, err := a.B.Views(r.Context(), r.URL.Query().Get("stats") == "1")
	if err != nil {
		fail(w, err)
		return
	}
	if views == nil {
		views = []api.View{}
	}
	writeJSON(w, http.StatusOK, views)
}

// load returns the server named in the path, or answers 404.
func (a *Agent) load(w http.ResponseWriter, r *http.Request) (*server.Server, bool) {
	s, err := a.B.Server(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return s, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "", "invalid request body: "+err.Error())
		return false
	}
	return true
}

func (a *Agent) get(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, s)
	}
}

func (a *Agent) view(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, a.B.View(r.Context(), s, r.URL.Query().Get("stats") == "1"))
	}
}

func (a *Agent) busy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"Busy": a.B.Busy(r.PathValue("id"))})
}

func (a *Agent) disk(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]int64{"Bytes": a.B.DiskUsage(s)})
	}
}

func (a *Agent) create(w http.ResponseWriter, r *http.Request) {
	var s server.Server
	if !decode(w, r, &s) {
		return
	}
	if err := a.B.Create(&s); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, &s)
}

func flag(r *http.Request, name string) bool { return r.URL.Query().Get(name) == "1" }

func (a *Agent) apply(w http.ResponseWriter, r *http.Request) {
	var s server.Server
	if !decode(w, r, &s) {
		return
	}
	if s.ID != r.PathValue("id") {
		writeErr(w, http.StatusBadRequest, "", "server id does not match the path")
		return
	}
	if err := a.B.Apply(&s, flag(r, "pull"), flag(r, "restart")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, &s)
}

func (a *Agent) update(w http.ResponseWriter, r *http.Request) {
	var s server.Server
	if !decode(w, r, &s) {
		return
	}
	if s.ID != r.PathValue("id") {
		writeErr(w, http.StatusBadRequest, "", "server id does not match the path")
		return
	}
	if err := a.B.ApplyUpdate(&s, flag(r, "pull"), flag(r, "restart"), flag(r, "backup")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, &s)
}

func (a *Agent) uninstall(w http.ResponseWriter, r *http.Request) {
	if err := a.B.Uninstall(r.PathValue("id"), r.URL.Query().Get("mode")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Agent) lifecycle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var fn func(string) error
	switch r.PathValue("action") {
	case "forget": // drop cached RCON results after a player list change
		a.B.Forget(id)
		w.WriteHeader(http.StatusNoContent)
		return
	case "start":
		fn = a.B.Start
	case "stop":
		fn = a.B.Stop
	case "restart":
		fn = a.B.Restart
	default:
		writeErr(w, http.StatusNotFound, "", "unknown action")
		return
	}
	if err := fn(id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Agent) nextPort(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"Port": a.B.NextPort(r.URL.Query().Get("exclude"))})
}

func (a *Agent) portTaken(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.URL.Query().Get("port"))
	writeJSON(w, http.StatusOK, map[string]bool{"Taken": a.B.PortTaken(port, r.URL.Query().Get("exclude"))})
}

func (a *Agent) discover(w http.ResponseWriter, r *http.Request) {
	list := a.B.Discover(r.Context())
	if list == nil {
		list = []api.Unmanaged{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Agent) importContainer(w http.ResponseWriter, r *http.Request) {
	var req api.ImportRequest
	if !decode(w, r, &req) {
		return
	}
	s, err := a.B.Import(r.Context(), req.ContainerID, req.Name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *Agent) rcon(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	var req api.RCONRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := a.B.RCON(r.Context(), s, req.Cmd)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.RCONReply{Out: out})
}

func (a *Agent) players(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	names, max, err := a.B.Players(r.Context(), s)
	if err != nil {
		fail(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, api.Players{Names: names, Max: max})
}

func (a *Agent) perf(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	p, err := a.B.Perf(r.Context(), s)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *Agent) container(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	c, err := a.B.Inspect(r.Context(), s)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ndjson starts a newline-delimited JSON stream; emit writes one value and flushes it right away.
// The request context ends the stream when the dashboard goes away.
func ndjson(w http.ResponseWriter) (emit func(v any) bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	fl, _ := w.(http.Flusher)
	return func(v any) bool {
		if enc.Encode(v) != nil || bw.Flush() != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
}

func (a *Agent) stats(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	// Check the container first so a missing one is a clean 404 instead of an empty stream.
	if _, err := a.B.Inspect(r.Context(), s); err != nil {
		fail(w, err)
		return
	}
	emit := ndjson(w)
	_ = a.B.StatsStream(r.Context(), s, func(st docker.Stats) { emit(st) })
}

func (a *Agent) logs(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tail, err := strconv.Atoi(q.Get("tail"))
	if err != nil {
		tail = -1
	}
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	if _, err := a.B.Inspect(r.Context(), s); err != nil {
		fail(w, err)
		return
	}
	emit := ndjson(w)
	_ = a.B.LogsSince(r.Context(), s, q.Get("follow") == "1", tail, since, func(l string) { emit(l) })
}

func (a *Agent) ops(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.B.ServerExists(id) {
		fail(w, server.ErrNotFound)
		return
	}
	emit := ndjson(w)
	a.B.FollowOps(r.Context(), id, func(l string) { emit(l) })
}
