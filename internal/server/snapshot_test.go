package server

import "testing"

func TestSnapshot(t *testing.T) {
	s := &Server{Kind: KindModrinth, Type: "NEOFORGE", Version: "1.21.1",
		Pack: &Pack{Slug: "atm10", Name: "ATM10", Version: "abc", VersionLabel: "4.12", MC: "1.21.1"}}
	old := s.Snapshot()
	s.Pack.Version, s.Pack.VersionLabel = "def", "4.15"
	if got := old.Label(); got != "ATM10 4.12 · MC 1.21.1" {
		t.Errorf("label = %q", got)
	}
	if old.SameSoftware(s.Snapshot()) {
		t.Error("different pack versions count as the same software")
	}
	if !old.SameSoftware(old) {
		t.Error("a snapshot differs from itself")
	}
	if old.SameSoftware((&Server{Type: "PAPER", Version: "1.21.1"}).Snapshot()) {
		t.Error("pack and plain server count as the same software")
	}
	var none *Snapshot
	if none.Label() != "" {
		t.Error("nil snapshot has a label")
	}
}

func TestRollback(t *testing.T) {
	s := &Server{ID: "a", Kind: KindModrinth, Type: "FABRIC", Version: "1.21.4", JavaTag: "java21", MemoryGB: 6,
		Pack: &Pack{Slug: "atm", Name: "ATM", Version: "new", VersionLabel: "2", MC: "1.21.4", Loader: "Fabric"}}
	old := func(f func(*Snapshot)) *Snapshot {
		sn := &Snapshot{Kind: KindModrinth, Type: "FABRIC", Version: "1.20.4", JavaTag: "java17",
			Pack: &Pack{Slug: "atm", Version: "old", VersionLabel: "1", MC: "1.20.4", Loader: "Fabric"}}
		if f != nil {
			f(sn)
		}
		return sn
	}
	if !s.CanRollback(old(nil)) {
		t.Fatal("rollback refused")
	}
	got := s.Rollback(old(nil))
	if got.Pack.Version != "old" || got.Pack.Name != "ATM" || got.Version != "1.20.4" || got.JavaTag != "java17" || got.MemoryGB != 6 {
		t.Fatalf("rollback = %+v %+v", got, got.Pack)
	}
	if s.Pack.Version != "new" || s.Version != "1.21.4" {
		t.Fatal("the current definition was changed")
	}
	for name, f := range map[string]func(*Snapshot){
		"other pack":       func(sn *Snapshot) { sn.Pack.Slug = "other" },
		"other kind":       func(sn *Snapshot) { sn.Kind = KindCurseForge },
		"no pack":          func(sn *Snapshot) { sn.Pack = nil },
		"same version":     func(sn *Snapshot) { sn.Pack.Version = "new" },
		"unpinned":         func(sn *Snapshot) { sn.Pack.Version = "" },
		"injected version": func(sn *Snapshot) { sn.Pack.Version = "1\n    privileged: true" },
		"injected image":   func(sn *Snapshot) { sn.JavaTag = "latest\n    x: y" },
		"injected type":    func(sn *Snapshot) { sn.Type = "A B" },
	} {
		if s.CanRollback(old(f)) {
			t.Errorf("%s: rollback allowed", name)
		}
	}
	soft := &Server{Kind: KindSoftware, Type: "PAPER", Version: "1.21.4"}
	if !soft.CanRollback(&Snapshot{Kind: KindSoftware, Type: "FABRIC", Version: "1.21.1"}) {
		t.Error("software rollback refused")
	}
	if soft.CanRollback(&Snapshot{Kind: KindSoftware, Type: "PAPER", Version: "LATEST"}) {
		t.Error("rollback to LATEST would not pin a version")
	}
}
