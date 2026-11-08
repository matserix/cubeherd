package web

import (
	"context"
	"net/http"

	"cubeherd/internal/access"
	"cubeherd/internal/api"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
)

// server loads a definition from the current node and marks where it came from, so links built from it
// point at the right node.
func (a *App) server(id string) (*server.Server, error) {
	s, err := a.M.Server(id)
	if err == nil {
		s.Node, s.NodeName = a.NodeID, a.NodeName
	}
	return s, err
}

// view is the node's view of a server, with the node marked on it.
func (a *App) view(ctx context.Context, s *server.Server, withStats bool) manager.View {
	v := a.M.View(ctx, s, withStats)
	if v.S != nil {
		v.S.Node, v.S.NodeName = a.NodeID, a.NodeName
		v.Can = capsOf(principalFrom(ctx), a.NodeID, v.S.ID)
	}
	return v
}

// caps is what the user behind ctx may do with a server on this node.
func (a *App) caps(ctx context.Context, id string) api.Caps {
	return capsOf(principalFrom(ctx), a.NodeID, id)
}

func capsOf(p *access.Principal, node, id string) api.Caps {
	c := func(perm access.Perm) bool { return p.Can(perm, node, id) }
	return api.Caps{Power: c(access.ServerPower), Console: c(access.ServerConsole), Command: c(access.ServerCommand),
		Players: c(access.ServerPlayers), Settings: c(access.ServerSettings) || c(access.ServerResources) || c(access.ServerNetwork) || c(access.ServerAdvanced), Rules: c(access.ServerSettings), Resources: c(access.ServerResources), Network: c(access.ServerNetwork), Advanced: c(access.ServerAdvanced), Update: c(access.ServerUpdate), Delete: c(access.ServerDelete),
		FilesRead: c(access.FilesRead), FilesWrite: c(access.FilesWrite), WorldExport: c(access.WorldExport), WorldImport: c(access.WorldImport),
		Addons: c(access.AddonsManage), BackupsView: c(access.BackupsView), BackupsCreate: c(access.BackupsCreate),
		BackupsRestore: c(access.BackupsRestore), BackupsDelete: c(access.BackupsDelete), Stats: c(access.StatsServer)}
}

// nodeSettings are the per-node defaults: the local node uses the global settings, a remote one its own.
func (a *App) nodeSettings() (pmin, pmax int, ips []string, def string) {
	cfg := a.Cfg.Get()
	pmin, pmax, ips, def = cfg.PortMin, cfg.PortMax, cfg.BindIPs, cfg.DefaultBindIP
	if info, ok := a.Reg.Get(a.NodeID); ok && !info.Local {
		e := info.Entry
		if e.PortMin > 0 && e.PortMax >= e.PortMin {
			pmin, pmax = e.PortMin, e.PortMax
		}
		ips, def = e.BindIPs, e.DefaultBindIP
	}
	return
}

func (a *App) portRange() (int, int) { pmin, pmax, _, _ := a.nodeSettings(); return pmin, pmax }
func (a *App) bindIPs() []string     { _, _, ips, _ := a.nodeSettings(); return ips }
func (a *App) defaultBindIP() string { _, _, _, def := a.nodeSettings(); return def }

// wizardEntry serves /servers/new: a node's wizard when the URL names the node (/n/<node>/s/new), else the
// node picker, which skips itself when there is only one node.
func (a *App) wizardEntry(w http.ResponseWriter, r *http.Request) {
	if _, named := explicitNode(r); named {
		a.h((*App).wizardPage)(w, r)
		return
	}
	a.p((*App).nodePick)(w, r)
}
