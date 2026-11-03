package web

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/config"
)

// auditCategories are the action prefixes offered as a filter.
var auditCategories = []struct{ Prefix, Label string }{
	{"login", "Anmeldung"}, {"logout", "Abmeldung"}, {"user.", "Benutzer"}, {"group.", "Gruppen"}, {"role.", "Rollen"},
	{"grant.", "Zuweisungen"}, {"server.", "Server"}, {"files.", "Dateien"}, {"world.", "Welt"}, {"addons.", "Mods und Plugins"},
	{"backup.", "Backups"}, {"node.", "Nodes"}, {"settings.", "Einstellungen"}, {"sso.", "Anmeldeanbieter"}, {"audit.", "Audit-Log"},
}

type auditRow struct {
	access.Entry
	NodeName   string
	ServerName string
}

type auditView struct {
	Rows     []auditRow
	Total    int
	Page     int
	Pages    int
	F        auditForm
	Query    string // the filter as a query string, for paging and export
	Global   bool   // the viewer sees the whole log
	Export   bool
	Verify   *access.VerifyResult
	Days     int
	Count    int
	Manage   bool
	Nodes    []scopeNode
	Cats     []struct{ Prefix, Label string }
	Size     int64
	PageSize int
}

type auditForm struct {
	Actor, Action, Outcome, Node, Text, From, To string
}

const auditPageSize = 50

func (a *App) auditFilter(r *http.Request) (access.Filter, auditForm, string) {
	p := principalOf(r)
	f := auditForm{Actor: r.FormValue("actor"), Action: r.FormValue("action"), Outcome: r.FormValue("outcome"), Node: r.FormValue("node"),
		Text: strings.TrimSpace(r.FormValue("q")), From: r.FormValue("from"), To: r.FormValue("to")}
	flt := access.Filter{Actor: f.Actor, Action: f.Action, Outcome: f.Outcome, Node: f.Node, Text: f.Text}
	if t, err := time.ParseInLocation("2006-01-02", f.From, displayLoc()); err == nil {
		flt.From = t
	}
	if t, err := time.ParseInLocation("2006-01-02", f.To, displayLoc()); err == nil {
		flt.To = t.Add(24 * time.Hour)
	}
	if !p.Global(access.AuditView) {
		flt.Only = p.Scopes(access.AuditView)
	}
	q := url.Values{}
	for k, v := range map[string]string{"actor": f.Actor, "action": f.Action, "outcome": f.Outcome, "node": f.Node, "q": f.Text, "from": f.From, "to": f.To} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return flt, f, q.Encode()
}

func (a *App) auditPage(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	flt, f, q := a.auditFilter(r)
	page, _ := strconv.Atoi(r.FormValue("page"))
	page = max(page, 1)
	flt.Limit, flt.Offset = auditPageSize, (page-1)*auditPageSize
	es, total, err := st.Audit(flt)
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	v := &auditView{Total: total, Page: page, Pages: max((total+auditPageSize-1)/auditPageSize, 1), F: f, Query: q,
		Global: p.Global(access.AuditView), Export: p.Anywhere(access.AuditExport), Manage: p.Global(access.UsersManage),
		Days: a.Cfg.Get().AuditDays, Count: st.AuditCount(), Cats: auditCategories, Size: st.Size(), PageSize: auditPageSize}
	if v.Days == 0 {
		v.Days = 365
	}
	for _, e := range es {
		row := auditRow{Entry: e}
		if e.Node != "" {
			row.NodeName = a.nodeName(e.Node)
		}
		if e.Server != "" {
			row.ServerName = a.serverName(e.Node, e.Server)
		}
		v.Rows = append(v.Rows, row)
	}
	for _, n := range a.Reg.List() {
		v.Nodes = append(v.Nodes, scopeNode{ID: n.ID, Name: n.Name})
	}
	if r.FormValue("verified") == "1" {
		res, _ := st.VerifyAudit()
		v.Verify = &res
	}
	a.accessPage(w, r, "audit", accessData{Audit: v})
}

func (a *App) auditEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	e, err := a.store().AuditEntry(id)
	flt, _, _ := a.auditFilter(r)
	if err != nil || !flt.Allows(e) {
		http.NotFound(w, r)
		return
	}
	detail := e.Detail
	if detail != "" {
		var m any
		if json.Unmarshal([]byte(detail), &m) == nil {
			if b, err := json.MarshalIndent(m, "", "  "); err == nil {
				detail = string(b)
			}
		}
	}
	a.partial(w, "access", "auditEntry", struct {
		E         access.Entry
		Detail    string
		Node, Srv string
	}{e, detail, a.nodeName(e.Node), a.serverName(e.Node, e.Server)})
}

func (a *App) auditExport(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	flt, _, _ := a.auditFilter(r)
	// an export is limited to the scopes the user may export, which can be narrower than the ones they may view
	if !p.Global(access.AuditExport) {
		flt.Only = p.Scopes(access.AuditExport)
	}
	format := r.FormValue("format")
	name := "cubeherd-audit-" + time.Now().Format("20060102-1504")
	a.record(r, "audit.export", format, access.OK)
	if format == "json" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.jsonl"`)
		_ = st.AuditAll(flt, func(e access.Entry) error {
			_, err := w.Write(append(e.JSON(), '\n'))
			return err
		})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "time", "actor", "ip", "action", "outcome", "node", "server", "target", "detail"})
	_ = st.AuditAll(flt, func(e access.Entry) error {
		return cw.Write([]string{strconv.FormatInt(e.ID, 10), e.Time.UTC().Format(time.RFC3339), csvSafe(e.Actor), e.IP, e.Action, e.Outcome, e.Node, e.Server, csvSafe(e.Target), csvSafe(e.Detail)})
	})
	cw.Flush()
}

// csvSafe keeps spreadsheet programs from running a cell as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *App) auditVerify(w http.ResponseWriter, r *http.Request) {
	res, err := a.store().VerifyAudit()
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	if res.OK {
		a.toast(w, "ok", "Die Kette ist unversehrt (%d Einträge).", res.Entries)
	} else {
		a.record(r, "audit.verify", fmt.Sprint(res.BadID), access.Fail, "reason", res.Reason)
		toast(w, a.t("Die Kette ist gebrochen bei Eintrag %d: %s", res.BadID, res.Reason), "crit")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) auditRetention(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < 0 || days > 3650 {
		a.fail(w, "Bitte gib eine Anzahl Tage zwischen 0 und 3650 an.")
		return
	}
	if days == 0 {
		days = -1 // keep forever
	}
	if err := a.Cfg.Update(func(s *config.Settings) { s.AuditDays = days }); err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	a.toast(w, "ok", "Gespeichert.")
	w.WriteHeader(http.StatusNoContent)
}
