package access

import (
	"slices"
	"time"
)

type held struct {
	scope Scope
	perms map[Perm]bool
}

// Principal is what a signed-in user may do. A nil Principal is the single-user mode, where the one
// password holder may do everything.
type Principal struct {
	User   User
	grants []held
}

// Principal resolves the user's current grants (direct and through groups, expired ones left out).
func (s *Store) Principal(u User) (*Principal, error) {
	gs, err := s.GrantsOf(u.ID)
	if err != nil {
		return nil, err
	}
	rp, err := s.rolePerms()
	if err != nil {
		return nil, err
	}
	p := &Principal{User: u}
	for _, g := range gs {
		if !g.Expires.IsZero() && g.Expires.Before(time.Now()) {
			continue
		}
		set := map[Perm]bool{}
		for _, pm := range rp[g.RoleID] {
			set[pm] = true
		}
		p.grants = append(p.grants, held{g.Scope, set})
	}
	return p, nil
}

// effective reports whether a grant at scope sc counts for perm: the permission must be allowed to live at that width.
func effective(sc Scope, perm Perm) bool {
	i, ok := Lookup(perm)
	return ok && sc.Level <= i.Lowest
}

// Can reports whether the user may use perm on the given node and server. Pass "" for the node to ask about the
// whole dashboard, and "" for the server to ask about a node. A wider grant always covers a narrower question.
func (p *Principal) Can(perm Perm, node, server string) bool {
	if p == nil {
		return true
	}
	for _, g := range p.grants {
		if !g.perms[perm] || !effective(g.scope, perm) {
			continue
		}
		switch g.scope.Level {
		case LevelGlobal:
			return true
		case LevelNode:
			if node != "" && g.scope.Node == node {
				return true
			}
		case LevelServer:
			if node != "" && server != "" && g.scope.Node == node && g.scope.Server == server {
				return true
			}
		}
	}
	return false
}

// Global is Can on the whole dashboard.
func (p *Principal) Global(perm Perm) bool { return p.Can(perm, "", "") }

// Anywhere reports whether the user holds perm on at least one scope.
func (p *Principal) Anywhere(perm Perm) bool {
	if p == nil {
		return true
	}
	return slices.ContainsFunc(p.grants, func(g held) bool { return g.perms[perm] && effective(g.scope, perm) })
}

// SeesNode reports whether any grant touches the node (so it appears in lists even without a server the user may see).
func (p *Principal) SeesNode(node string) bool {
	if p == nil {
		return true
	}
	return slices.ContainsFunc(p.grants, func(g held) bool {
		return len(g.perms) > 0 && (g.scope.Level == LevelGlobal || g.scope.Node == node)
	})
}

// NodeWide reports whether a grant on the whole dashboard or on this node (not just single servers) gives perm.
func (p *Principal) NodeWide(perm Perm, node string) bool { return p.Can(perm, node, "") }

// CanGrant reports whether the user may hand out role r on scope sc: every permission that would take effect
// there must be one the user holds on that scope.
func (p *Principal) CanGrant(r Role, sc Scope) bool {
	if p == nil {
		return true
	}
	for _, pm := range r.Perms {
		if !effective(sc, pm) {
			continue
		}
		if !p.Can(pm, sc.Node, sc.Server) {
			return false
		}
	}
	return true
}

// CanDefine reports whether the user holds every permission of a role globally, which is what editing a role needs.
func (p *Principal) CanDefine(perms []Perm) bool {
	if p == nil {
		return true
	}
	for _, pm := range perms {
		if !p.Global(pm) {
			return false
		}
	}
	return true
}

// Summary lists what the user's grants give, for the "effective access" view.
type Summary struct {
	Scope Scope
	Perms []Perm
}

func (p *Principal) Summary() []Summary {
	var out []Summary
	for _, g := range p.grants {
		var ps []Perm
		for _, i := range Catalog {
			if g.perms[i.ID] && effective(g.scope, i.ID) {
				ps = append(ps, i.ID)
			}
		}
		out = append(out, Summary{g.scope, ps})
	}
	return out
}

// Scopes lists the scopes on which the user holds perm, for views that must be limited to them (the audit log).
func (p *Principal) Scopes(perm Perm) []Scope {
	var out []Scope
	for _, g := range p.grants {
		if g.perms[perm] && effective(g.scope, perm) {
			out = append(out, g.scope)
		}
	}
	return out
}

// Allows reports whether an entry lies inside Only (always true when Only is nil).
func (f Filter) Allows(e Entry) bool {
	if f.Only == nil {
		return true
	}
	for _, sc := range f.Only {
		switch sc.Level {
		case LevelGlobal:
			return true
		case LevelNode:
			if e.Node == sc.Node {
				return true
			}
		case LevelServer:
			if e.Node == sc.Node && e.Server == sc.Server {
				return true
			}
		}
	}
	return false
}

// Expired reports whether a grant no longer applies.
func (g Grant) Expired() bool { return !g.Expires.IsZero() && g.Expires.Before(time.Now()) }
