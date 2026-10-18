package web

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"forgecraft/internal/manager"
	"forgecraft/internal/server"
)

type backupsData struct {
	ID      string
	Plan    server.BackupPlan
	List    []manager.Backup
	Total   int64
	Next    time.Time
	Busy    bool // a backup/restore runs: the list refreshes itself
	Running bool
}

func (a *App) backupsData(s *server.Server, st server.Status) backupsData {
	d := backupsData{ID: s.ID, Plan: s.Plan(), Next: a.M.NextBackupFor(s), Busy: st.Busy(), Running: st.Up()}
	d.List, _ = a.M.Backups(s)
	for _, b := range d.List {
		d.Total += b.Size
	}
	return d
}

func (a *App) backupList(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	v := a.M.View(r.Context(), s, false)
	a.partial(w, "server", "backupList", a.backupsData(s, v.Status))
}

func (a *App) backupCreate(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	live := a.M.View(r.Context(), s, false).Status.Live()
	if err := a.M.BackupNow(s.ID, manager.ReasonManual); err != nil {
		fail(w, a.t(err.Error()))
		return
	}
	if live {
		a.toast(w, "", "Backup läuft. Spieler bleiben verbunden.")
	} else {
		a.toast(w, "", "Backup läuft.")
	}
	time.Sleep(150 * time.Millisecond)
	a.backupList(w, r)
}

var clock = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (a *App) backupPlan(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	p := server.BackupPlan{
		Interval: r.FormValue("interval"), Time: strings.TrimSpace(r.FormValue("time")),
		Keep: formInt(r, "keep", 7, 1, 100), BeforeUpdate: checked(r, "beforeUpdate"), OnlyIfActive: checked(r, "onlyIfActive"),
	}
	switch p.Interval {
	case "off", "6h", "daily", "weekly":
	default:
		a.fail(w, "Unbekanntes Intervall.")
		return
	}
	if !clock.MatchString(p.Time) {
		a.fail(w, "Uhrzeit bitte als HH:MM angeben, z. B. 04:00.")
		return
	}
	s.Backup = &p
	if err := a.M.Apply(s, false, false); err != nil {
		fail(w, a.t(err.Error()))
		return
	}
	a.M.WakeScheduler()
	if next := a.M.NextBackupFor(s); !next.IsZero() {
		a.toast(w, "ok", "Zeitplan gespeichert. Nächstes Backup: %s.", next.In(a.M.Loc()).Format("2006-01-02 15:04"))
	} else {
		a.toast(w, "ok", "Zeitplan gespeichert.")
	}
	a.partial(w, "server", "backupList", a.backupsData(s, a.M.View(r.Context(), s, false).Status))
}

func (a *App) backupDownload(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	p, err := a.M.BackupPath(s, r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, s.ID, r.PathValue("name")))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (a *App) backupDelete(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	if err := a.M.DeleteBackup(s, r.PathValue("name")); err != nil {
		a.fail(w, "Löschen fehlgeschlagen: %s", err.Error())
		return
	}
	a.toast(w, "ok", "Backup gelöscht.")
	a.backupList(w, r)
}

func (a *App) restoreModal(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	list, _ := a.M.Backups(s)
	for _, b := range list {
		if b.Name == name {
			a.partial(w, "server", "restoreModal", map[string]any{"S": s, "B": b, "Pack": s.Pack != nil})
			return
		}
	}
	a.fail(w, "Backup nicht gefunden.")
}

func (a *App) restorePost(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	if err := a.M.Restore(s.ID, r.PathValue("name")); err != nil {
		fail(w, a.t(err.Error()))
		return
	}
	redirect(w, r, "/servers/"+s.ID+"?tab=console")
}
