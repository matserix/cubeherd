package web

import (
	"context"
	"net/http"
	"slices"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/api"
	"cubeherd/internal/nodes"
)

// refresh is Reg.Refresh limited to what the signed-in user may see: servers without server.view are dropped, and a
// node stays only if the user may look at nodes, create servers on it or still sees a server on it.
func (a *App) refresh(ctx context.Context, withStats bool, maxAge time.Duration) []nodes.View {
	all := a.Reg.Refresh(ctx, withStats, maxAge)
	p := principalFrom(ctx)
	out := make([]nodes.View, 0, len(all))
	for _, n := range all {
		views := make([]api.View, 0, len(n.Views))
		for _, v := range n.Views {
			if p.Can(access.ServerView, n.ID, v.S.ID) {
				v.Can = capsOf(p, n.ID, v.S.ID)
				views = append(views, v)
			}
		}
		if p != nil && len(views) == 0 && !p.Can(access.NodesView, n.ID, "") && !p.Can(access.ServerCreate, n.ID, "") {
			continue
		}
		n.Views = views
		out = append(out, n)
	}
	return out
}

// visibleNodes are the nodes the user may look at (nodes.view), fully refreshed, for the Nodes page.
func (a *App) visibleNodes(r *http.Request) []nodes.View {
	all := a.Reg.Refresh(r.Context(), false, 0)
	p := principalOf(r)
	return slices.DeleteFunc(all, func(n nodes.View) bool { return !p.Can(access.NodesView, n.ID, "") })
}
