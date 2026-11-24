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
