package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Compose renders the compose file for a server. Values are JSON-quoted (valid YAML)
// and "$" is doubled so Compose does not interpolate it.
func Compose(s *Server, cfKey string) string {
	q := func(v string) string {
		b, _ := json.Marshal(strings.ReplaceAll(v, "$", "$$"))
		return string(b)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Erzeugt von Forgecraft – Änderungen bitte im Dashboard vornehmen.\n")
	fmt.Fprintf(&b, "name: %s\n", s.Project())
	b.WriteString("services:\n  mc:\n")
	fmt.Fprintf(&b, "    image: %s\n", s.ImageRef())
	fmt.Fprintf(&b, "    container_name: %s\n", s.Container())
	b.WriteString("    environment:\n")
	for _, kv := range s.Env(cfKey) {
		fmt.Fprintf(&b, "      %s: %s\n", kv.Key, q(kv.Value))
	}
	fmt.Fprintf(&b, "    ports:\n      - %s\n", q(fmt.Sprintf("%d:25565", s.Port)))
	data := "./data"
	switch {
	case s.DataVol != "":
		data = s.DataVol
	case s.DataDir != "":
		data = s.DataDir
	}
	fmt.Fprintf(&b, "    volumes:\n      - %s\n", q(data+":/data"))
	fmt.Fprintf(&b, "    labels:\n      forgecraft.id: %s\n", q(s.ID))
	restart := "no"
	if s.Autostart {
		restart = "unless-stopped"
	}
	fmt.Fprintf(&b, "    restart: %s\n", q(restart))
	// MemoryGB is the container limit; the heap (MEMORY) is derived from it with headroom.
	fmt.Fprintf(&b, "    mem_limit: %s\n    memswap_limit: %s\n", q(fmt.Sprintf("%dg", s.MemoryGB)), q(fmt.Sprintf("%dg", s.MemoryGB)))
	b.WriteString("    stop_grace_period: 1m\n")
	b.WriteString("    tty: true\n    stdin_open: true\n")
	if s.DataVol != "" {
		fmt.Fprintf(&b, "volumes:\n  %s:\n    external: true\n", q(s.DataVol))
	}
	return b.String()
}
