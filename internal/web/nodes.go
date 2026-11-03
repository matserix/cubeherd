package web

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"cubeherd/internal/access"
	"cubeherd/internal/agent"
	"cubeherd/internal/config"
	"cubeherd/internal/nodes"
	"cubeherd/internal/remote"
)

// ---------------- Nodes ----------------

type nodesData struct {
	Rows []nodes.View
}

func (a *App) nodesPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "nodes", Page{Title: a.t("Nodes"), Nav: "nodes", Data: nodesData{Rows: a.visibleNodes(r)}})
}

// nodeError turns a connection error into a message for the add and edit dialogs.
func (a *App) nodeError(err error) string {
	switch {
	case errors.Is(err, remote.ErrUnauthorized):
		return a.t("Der Agent hat das Token abgelehnt.")
	case errors.Is(err, remote.ErrFingerprint):
		return a.t("Das Zertifikat des Agents stimmt nicht mehr mit dem gespeicherten Fingerabdruck überein.")
	case errors.Is(err, remote.ErrAPIVersion):
		return a.t("Der Agent spricht eine andere API-Version als das Dashboard.")
	}
	return a.t("Der Agent ist nicht erreichbar: %s", err.Error())
}

type nodeDialog struct {
	Name, Addr, Token, Fingerprint string
	Error                          string
	Version, Hostname              string // identity of the agent, once it has answered
	ID                             string // set when editing
	PortMin, PortMax               int
	BindIPs                        string
	DefaultBindIP                  string
}

// nodeNew is step 1 of adding a node: name, address and a token the dashboard makes up, plus the command for the agent host.
func (a *App) nodeNew(w http.ResponseWriter, r *http.Request) {
	tok, err := agent.GenerateToken()
	if err != nil {
		fail(w, err.Error())
		return
	}
	a.partial(w, "nodes", "nodeStep1", nodeDialog{Token: tok})
}

// nodeProbe is step 2: it fetches the agent's certificate fingerprint and checks the token against it. The
// operator confirms the fingerprint against what the agent printed on its host (trust on first use).
func (a *App) nodeProbe(w http.ResponseWriter, r *http.Request) {
	d := nodeDialog{Name: strings.TrimSpace(r.FormValue("name")), Addr: strings.TrimSpace(r.FormValue("addr")), Token: strings.TrimSpace(r.FormValue("token"))}
	if d.Name == "" || d.Addr == "" || d.Token == "" {
		d.Error = a.t("Name, Adresse und Token werden gebraucht.")
		a.partial(w, "nodes", "nodeStep1", d)
		return
	}
	fp, err := a.Reg.Probe(r.Context(), d.Addr)
	if err != nil {
		d.Error = a.t("Der Agent ist nicht erreichbar: %s", err.Error())
		a.partial(w, "nodes", "nodeStep1", d)
		return
	}
	in, err := a.Reg.Check(r.Context(), d.Addr, d.Token, fp)
	if err != nil {
		d.Error = a.nodeError(err)
		a.partial(w, "nodes", "nodeStep1", d)
		return
	}
	d.Fingerprint, d.Version, d.Hostname = fp, in.Version, in.Hostname
	a.partial(w, "nodes", "nodeStep2", d)
}

// nodeAdd stores the node once the operator has confirmed the fingerprint.
func (a *App) nodeAdd(w http.ResponseWriter, r *http.Request) {
	name, addr, token, fp := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("addr")), strings.TrimSpace(r.FormValue("token")), strings.TrimSpace(r.FormValue("fingerprint"))
	if r.FormValue("confirm") != "on" {
		a.fail(w, "Bitte bestätige, dass der Fingerabdruck mit dem des Agents übereinstimmt.")
		return
	}
	if _, err := a.Reg.Check(r.Context(), addr, token, fp); err != nil {
		fail(w, a.nodeError(err))
		return
	}
	switch _, err := a.Reg.Add(name, addr, token, fp); {
	case errors.Is(err, nodes.ErrExists):
		a.fail(w, "Ein Node mit diesem Namen gibt es schon.")
		return
	case err != nil:
		a.fail(w, "Node nicht gespeichert: %s", err.Error())
		return
	}
	go a.Reg.Refresh(r.Context(), false, 0) // pushes the settings to the new agent right away
	redirect(w, r, "/nodes")
}

func (a *App) nodeEntry(w http.ResponseWriter, r *http.Request) (nodes.Info, bool) {
	info, ok := a.Reg.Get(r.PathValue("id"))
	if !ok || info.Local {
		a.fail(w, "Dieser Node existiert nicht.")
		return info, false
	}
	return info, true
}

func (a *App) nodeEdit(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == nodes.LocalID {
		if info, ok := a.Reg.Get(nodes.LocalID); ok {
			a.partial(w, "nodes", "nodeEditLocal", info)
			return
		}
	}
	info, ok := a.nodeEntry(w, r)
	if !ok {
		return
	}
	e := info.Entry
	a.partial(w, "nodes", "nodeEdit", nodeDialog{ID: e.ID, Name: e.Name, Addr: e.Addr, Fingerprint: e.Fingerprint,
		PortMin: e.PortMin, PortMax: e.PortMax, BindIPs: strings.Join(e.BindIPs, "\n"), DefaultBindIP: e.DefaultBindIP})
}

// nodeReprobe fetches the agent's current fingerprint, for the case that its certificate was replaced.
func (a *App) nodeReprobe(w http.ResponseWriter, r *http.Request) {
	info, ok := a.nodeEntry(w, r)
	if !ok {
		return
	}
	addr := strings.TrimSpace(r.FormValue("addr"))
	if addr == "" {
		addr = info.Addr
	}
	fp, err := a.Reg.Probe(r.Context(), addr)
	if err != nil {
		fail(w, a.t("Der Agent ist nicht erreichbar: %s", err.Error()))
		return
	}
	a.partial(w, "nodes", "fpInput", fp)
}

// nodeSaveLocal renames the embedded node. Everything else about it is the global settings.
func (a *App) nodeSaveLocal(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if err := a.Cfg.Update(func(s *config.Settings) { s.LocalName = name }); err != nil {
		fail(w, a.t(err.Error()))
		return
	}
	redirect(w, r, "/nodes")
}

func (a *App) nodeSave(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == nodes.LocalID {
		a.nodeSaveLocal(w, r)
		return
	}
	info, ok := a.nodeEntry(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	addr := strings.TrimSpace(r.FormValue("addr"))
	fp := strings.TrimSpace(r.FormValue("fingerprint"))
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		token = info.Entry.Token // empty keeps the stored one
	}
	if name == "" || addr == "" || fp == "" {
		a.fail(w, "Name, Adresse und Fingerabdruck werden gebraucht.")
		return
	}
	pmin, pmax := formInt(r, "portMin", 0, 0, 65535), formInt(r, "portMax", 0, 0, 65535)
	if (pmin != 0 || pmax != 0) && (pmin < 1024 || pmax < pmin) {
		a.fail(w, "Der Port-Bereich ist ungültig.")
		return
	}
	ips, ierr := parseBindIPs(r.FormValue("bindIps"))
	if ierr != nil {
		a.fail(w, "„%s“ ist keine gültige IP-Adresse.", ierr.Error())
		return
	}
	def := strings.TrimSpace(r.FormValue("defaultBindIp"))
	if def != "" && !slices.Contains(ips, def) {
		a.fail(w, "Die Standard-IP muss in der Liste der IP-Adressen stehen.")
		return
	}
	if _, err := a.Reg.Check(r.Context(), addr, token, fp); err != nil {
		fail(w, a.nodeError(err))
		return
	}
	err := a.Reg.Edit(info.ID, func(n *config.Node) {
		n.Name, n.Addr, n.Token, n.Fingerprint = name, addr, token, fp
		n.PortMin, n.PortMax, n.BindIPs, n.DefaultBindIP = pmin, pmax, ips, def
	})
	if err != nil {
		fail(w, a.t(err.Error()))
		return
	}
	redirect(w, r, "/nodes")
}

func (a *App) nodeRemoveModal(w http.ResponseWriter, r *http.Request) {
	info, ok := a.nodeEntry(w, r)
	if !ok {
		return
	}
	a.partial(w, "nodes", "nodeRemove", info)
}

func (a *App) nodeRemove(w http.ResponseWriter, r *http.Request) {
	info, ok := a.nodeEntry(w, r)
	if !ok {
		return
	}
	if err := a.Reg.Remove(info.ID); err != nil {
		fail(w, a.t(err.Error()))
		return
	}
	redirect(w, r, "/nodes")
}

// ---------------- Node picker (placement) ----------------

// nodePick is the first step of the wizard: where should the server run? With one node there is nothing to pick.
func (a *App) nodePick(w http.ResponseWriter, r *http.Request) {
	all := a.refresh(r.Context(), false, 2e9)
	if p := principalOf(r); p != nil {
		all = slices.DeleteFunc(all, func(n nodes.View) bool { return !p.Can(access.ServerCreate, n.ID, "") })
	}
	if len(all) == 1 && all[0].Status.State != nodes.Offline {
		redirect(w, r, "/n/"+all[0].ID+"/s/new")
		return
	}
	a.render(w, r, "nodepick", Page{Title: a.t("Neuer Server"), Nav: "wizard", Data: nodesData{Rows: all}})
}
