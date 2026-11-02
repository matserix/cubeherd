package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/manager"
)

func (a *Agent) backupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/config", a.putConfig)
	mux.HandleFunc("GET /v1/backups/remote", a.hasRemote)
	mux.HandleFunc("POST /v1/backups/remote/invalidate", a.invalidateRemote)
	mux.HandleFunc("POST /v1/scheduler/wake", a.wakeScheduler)

	mux.HandleFunc("GET /v1/servers/{id}/backups", a.backups)
	mux.HandleFunc("POST /v1/servers/{id}/backups", a.backupNow)
	mux.HandleFunc("GET /v1/servers/{id}/backups/health", a.backupHealth)
	mux.HandleFunc("GET /v1/servers/{id}/backups/next", a.backupNext)
	mux.HandleFunc("GET /v1/servers/{id}/backups/{name}/download", a.backupDownload)
	mux.HandleFunc("POST /v1/servers/{id}/backups/{name}/{action}", a.backupAction)
	mux.HandleFunc("DELETE /v1/servers/{id}/backups/{name}", a.backupDelete)
}

func (a *Agent) putConfig(w http.ResponseWriter, r *http.Request) {
	var c api.Config
	if !decode(w, r, &c) {
		return
	}
	if err := a.B.ApplyConfig(c); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) hasRemote(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"Configured": a.B.HasRemote()})
}

func (a *Agent) invalidateRemote(w http.ResponseWriter, r *http.Request) {
	a.B.InvalidateRemote()
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) wakeScheduler(w http.ResponseWriter, r *http.Request) {
	a.B.WakeScheduler()
	w.WriteHeader(http.StatusNoContent)
}

func toWire(b manager.Backup) api.Backup {
	w := api.Backup{Name: b.Name, Time: b.Time, Reason: b.Reason, Size: b.Size, Local: b.Local, Remote: b.Remote, Check: b.Check, CheckErr: b.CheckErr}
	if b.UploadErr != nil {
		w.UploadErr = b.UploadErr.Error()
	}
	return w
}

func (a *Agent) backups(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	list, err := a.B.BackupsAll(r.Context(), s)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]api.Backup, 0, len(list))
	for _, b := range list {
		out = append(out, toWire(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Agent) backupNow(w http.ResponseWriter, r *http.Request) {
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = manager.ReasonManual
	}
	if err := a.B.BackupNow(r.PathValue("id"), reason); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Agent) backupHealth(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, a.B.BackupHealth(s, flag(r, "running")))
	}
}

func (a *Agent) backupNext(w http.ResponseWriter, r *http.Request) {
	if s, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]time.Time{"Next": a.B.NextBackupFor(s)})
	}
}

func (a *Agent) backupDownload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	name, _ := url.PathUnescape(r.PathValue("name"))
	body, size, mod, err := a.B.OpenBackup(r.Context(), s, name)
	if err != nil {
		fail(w, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(size))
	}
	if !mod.IsZero() {
		w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		panic(http.ErrAbortHandler) // truncated archive: make it visible
	}
}

func (a *Agent) backupAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, _ := url.PathUnescape(r.PathValue("name"))
	var fn func(string, string) error
	switch r.PathValue("action") {
	case "verify":
		fn = a.B.VerifyBackup
	case "upload":
		fn = a.B.UploadBackup
	case "restore":
		fn = a.B.Restore
	default:
		writeErr(w, http.StatusNotFound, "", "unknown action")
		return
	}
	if err := fn(id, name); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *Agent) backupDelete(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}
	name, _ := url.PathUnescape(r.PathValue("name"))
	if err := a.B.DeleteBackupEverywhere(r.Context(), s, name); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
