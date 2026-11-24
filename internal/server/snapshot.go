package server

import "strings"

// Snapshot is the part of a definition that tells which software a backup belongs to. It is stored inside
// every backup archive.
type Snapshot struct {
	Kind    string `json:"kind"`
	Type    string `json:"type"`
	Version string `json:"version"`
	JavaTag string `json:"javaTag,omitempty"`
	Pack    *Pack  `json:"pack,omitempty"`
}

// Snapshot returns a copy of the software settings, independent of later changes to s.
func (s *Server) Snapshot() *Snapshot {
	sn := &Snapshot{Kind: s.Kind, Type: s.Type, Version: s.Version, JavaTag: s.JavaTag}
	if s.Pack != nil {
		p := *s.Pack
		sn.Pack = &p
	}
	return sn
}

// Label is the short human readable form, e.g. "ATM10 4.12 · MC 1.21.1" or "Paper 1.21.4".
func (sn *Snapshot) Label() string {
	if sn == nil {
		return ""
	}
	if p := sn.Pack; p != nil {
		name := p.Name
		if name == "" {
			name = p.Slug
		}
		version := p.VersionLabel
		if version == "" {
			version = p.Version
		}
		label := strings.TrimSpace(name + " " + version)
		mc := p.MC
		if mc == "" {
			mc = sn.Version
		}
		if mc != "" {
			label += " · MC " + mc
		}
		return label
	}
	if sn.Type == "" {
		return sn.Version
	}
	return strings.TrimSpace(TypeName(sn.Type) + " " + sn.Version)
}

// SameSoftware reports whether both snapshots run the same software in the same version.
func (sn *Snapshot) SameSoftware(o *Snapshot) bool {
	if sn == nil || o == nil {
		return sn == o
	}
	if (sn.Pack == nil) != (o.Pack == nil) {
		return false
	}
	if sn.Pack != nil {
		return sn.Pack.Slug == o.Pack.Slug && sn.Pack.Version == o.Pack.Version
	}
	return sn.Type == o.Type && sn.Version == o.Version
}
