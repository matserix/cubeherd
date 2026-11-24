package server

import (
	"regexp"
	"strings"
)

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

// A snapshot is read from a file, and its values end up in the compose file; only plain identifiers pass.
var (
	snapType    = regexp.MustCompile(`^[A-Z_]{1,32}$`)
	snapVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	snapJavaTag = regexp.MustCompile(`^java\d{1,2}(-[a-z0-9]+)*$`)
)

// CanRollback reports whether the server can be set back to the software of sn: it must differ from the current
// one, be of the same kind (and the same modpack) and name exact versions.
func (s *Server) CanRollback(sn *Snapshot) bool {
	if sn == nil || sn.Kind != s.Kind || (sn.Pack == nil) != (s.Pack == nil) || sn.SameSoftware(s.Snapshot()) {
		return false
	}
	if !snapType.MatchString(sn.Type) || (sn.JavaTag != "" && !snapJavaTag.MatchString(sn.JavaTag)) {
		return false
	}
	exact := func(v string) bool { return snapVersion.MatchString(v) && !strings.EqualFold(v, "latest") }
	if p := sn.Pack; p != nil {
		return p.Slug == s.Pack.Slug && exact(p.Version) && (sn.Version == "" || snapVersion.MatchString(sn.Version))
	}
	return exact(sn.Version)
}

// Rollback returns a copy of the definition that runs the software of sn; s stays as it is. Everything else,
// including the modpack's name and icon, is kept. It must only be called when CanRollback is true.
func (s *Server) Rollback(sn *Snapshot) *Server {
	out := *s
	out.Type, out.Version = sn.Type, sn.Version
	if sn.JavaTag != "" {
		out.JavaTag = sn.JavaTag
	}
	if s.Pack != nil && sn.Pack != nil {
		p := *s.Pack
		p.Version, p.VersionLabel, p.MC, p.Loader = sn.Pack.Version, sn.Pack.VersionLabel, sn.Pack.MC, sn.Pack.Loader
		out.Pack = &p
	}
	return &out
}
