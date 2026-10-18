// Package server holds the server definition, its on-disk store and the
// translation into a compose file for itzg/minecraft-server.
package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	KindSoftware   = "software"
	KindModrinth   = "modrinth"
	KindCurseForge = "curseforge"
	Image          = "itzg/minecraft-server"
)

// Types lists the server software the wizard offers (itzg TYPE values).
var Types = []struct{ ID, Name, Desc string }{
	{"VANILLA", "Vanilla", "Offizieller Mojang-Server"},
	{"PAPER", "Paper", "Schnell, Bukkit-Plugins"},
	{"PURPUR", "Purpur", "Paper mit mehr Optionen"},
	{"FOLIA", "Folia", "Paper, mehrkernig"},
	{"FABRIC", "Fabric", "Leichter Mod-Loader"},
	{"QUILT", "Quilt", "Fabric-kompatibel"},
	{"FORGE", "Forge", "Klassischer Mod-Loader"},
	{"NEOFORGE", "NeoForge", "Forge-Nachfolger"},
}

func TypeName(id string) string {
	for _, t := range Types {
		if t.ID == id {
			return t.Name
		}
	}
	if id == "" {
		return "Unbekannt"
	}
	return strings.ToUpper(id[:1]) + strings.ToLower(id[1:])
}

// IsPluginType reports whether the software loads Bukkit plugins instead of mods.
func IsPluginType(t string) bool { return t == "PAPER" || t == "PURPUR" || t == "FOLIA" }

type Pack struct {
	Slug         string `json:"slug"`
	ProjectID    string `json:"projectId,omitempty"`
	Name         string `json:"name"`
	Version      string `json:"version"`      // Modrinth version id / CurseForge file id
	VersionLabel string `json:"versionLabel"` // human readable
	Loader       string `json:"loader"`
	MC           string `json:"mc"`
	IconURL      string `json:"iconUrl,omitempty"`
}

type Props struct {
	MOTD         string `json:"motd"`
	Mode         string `json:"mode"`
	Difficulty   string `json:"difficulty"`
	MaxPlayers   int    `json:"maxPlayers"`
	ViewDistance int    `json:"viewDistance"`
	SimDistance  int    `json:"simDistance"`
	Seed         string `json:"seed"`
	PVP          bool   `json:"pvp"`
	Whitelist    bool   `json:"whitelist"`
	OnlineMode   bool   `json:"onlineMode"`
}

type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Server struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Type      string    `json:"type"`    // itzg TYPE for software servers, loader for packs
	Version   string    `json:"version"` // Minecraft version
	Pack      *Pack     `json:"pack,omitempty"`
	JavaTag   string    `json:"javaTag"`
	Port      int       `json:"port"`
	MemoryGB  int       `json:"memoryGb"`
	Autopause bool      `json:"autopause"`
	Aikar     bool      `json:"aikar"`
	Autostart bool      `json:"autostart"`
	Props     Props     `json:"props"`
	Whitelist []string  `json:"whitelist"`
	Ops       []string  `json:"ops"`
	ExtraEnv  []KV      `json:"extraEnv"`
	DataDir   string    `json:"dataDir,omitempty"`    // imported servers whose data lives in another host folder
	DataVol   string    `json:"dataVolume,omitempty"` // imported servers whose data lives in a Docker volume
	Imported  bool      `json:"imported,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s *Server) Container() string { return "mc-" + s.ID }

// Modded reports whether the server loads mods (more memory outside the Java heap).
func (s *Server) Modded() bool {
	return s.Pack != nil || (s.Type != "VANILLA" && !IsPluginType(s.Type))
}

// HeapMB is the Java heap that fits into the container limit (MemoryGB). The rest is left for
// metaspace, threads, native buffers and the container OS; modpacks need noticeably more of it.
func (s *Server) HeapMB() int { return HeapMB(s.MemoryGB, s.Modded()) }

func HeapMB(totalGB int, modded bool) int {
	total := totalGB * 1024
	overhead := max(1024, total/5)
	if modded {
		// Measured with ATM10 (400+ mods): ~1.7 GB outside the heap at idle.
		overhead = max(2048, total*3/10)
	}
	heap := (total - overhead) / 256 * 256
	return max(heap, 512)
}
func (s *Server) Project() string { return "mc-" + s.ID }

// SoftwareLabel is the short description shown in lists, e.g. "Paper 1.21.10".
func (s *Server) SoftwareLabel() string { return TypeName(s.Type) + " " + s.Version }

func (s *Server) ImageRef() string {
	tag := s.JavaTag
	if tag == "" {
		tag = "latest"
	}
	return Image + ":" + tag
}

// Env builds the container environment in a stable order.
func (s *Server) Env(cfKey string) []KV {
	e := []KV{{"EULA", "TRUE"}}
	switch s.Kind {
	case KindModrinth:
		e = append(e, KV{"TYPE", "MODRINTH"}, KV{"MODRINTH_MODPACK", s.Pack.Slug})
		if s.Pack.Version != "" {
			e = append(e, KV{"MODRINTH_VERSION", s.Pack.Version})
		}
	case KindCurseForge:
		e = append(e, KV{"TYPE", "AUTO_CURSEFORGE"}, KV{"CF_SLUG", s.Pack.Slug})
		if s.Pack.Version != "" {
			e = append(e, KV{"CF_FILE_ID", s.Pack.Version})
		}
		if cfKey != "" {
			e = append(e, KV{"CF_API_KEY", cfKey})
		}
	default:
		e = append(e, KV{"TYPE", s.Type}, KV{"VERSION", s.Version})
	}
	e = append(e, KV{"MEMORY", fmt.Sprintf("%dM", s.HeapMB())})
	if s.Aikar {
		e = append(e, KV{"USE_AIKAR_FLAGS", "TRUE"})
	}
	if s.Autopause {
		// After SIGCONT the server sees a huge tick gap; without this the watchdogs kill it.
		e = append(e, KV{"ENABLE_AUTOPAUSE", "TRUE"}, KV{"MAX_TICK_TIME", "-1"})
		if IsPluginType(s.Type) {
			e = append(e, KV{"JVM_DD_OPTS", "disable.watchdog:true"})
		}
	}
	if s.Modded() {
		// Many threads make glibc create many malloc arenas; capping them saves native memory.
		e = append(e, KV{"MALLOC_ARENA_MAX", "2"})
	}
	p := s.Props
	add := func(k, v string) {
		if v != "" {
			e = append(e, KV{k, v})
		}
	}
	add("MOTD", p.MOTD)
	add("MODE", p.Mode)
	add("DIFFICULTY", p.Difficulty)
	if p.MaxPlayers > 0 {
		add("MAX_PLAYERS", fmt.Sprint(p.MaxPlayers))
	}
	if p.ViewDistance > 0 {
		add("VIEW_DISTANCE", fmt.Sprint(p.ViewDistance))
	}
	if p.SimDistance > 0 {
		add("SIMULATION_DISTANCE", fmt.Sprint(p.SimDistance))
	}
	add("SEED", p.Seed)
	add("PVP", boolEnv(p.PVP))
	add("ONLINE_MODE", boolEnv(p.OnlineMode))
	if p.Whitelist {
		add("ENABLE_WHITELIST", "TRUE")
	}
	// The env lists are authoritative: entries removed in Forgecraft disappear on the next start.
	if len(s.Whitelist) > 0 {
		add("WHITELIST", strings.Join(s.Whitelist, ","))
		add("EXISTING_WHITELIST_FILE", "SYNCHRONIZE")
	}
	if len(s.Ops) > 0 {
		add("OPS", strings.Join(s.Ops, ","))
		add("EXISTING_OPS_FILE", "SYNCHRONIZE")
	}
	// Extra variables override generated ones with the same key.
	for _, kv := range s.ExtraEnv {
		replaced := false
		for i := range e {
			if e[i].Key == kv.Key {
				e[i].Value, replaced = kv.Value, true
			}
		}
		if !replaced {
			e = append(e, kv)
		}
	}
	return e
}

func boolEnv(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnv parses KEY=VALUE lines; blank lines and # comments are ignored.
func ParseEnv(text string) ([]KV, error) {
	var out []KV
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envKey.MatchString(k) {
			return nil, fmt.Errorf("Zeile %d: erwartet KEY=WERT", i+1)
		}
		out = append(out, KV{k, v})
	}
	return out, nil
}

func FormatEnv(kvs []KV) string {
	var b strings.Builder
	for _, kv := range kvs {
		b.WriteString(kv.Key + "=" + kv.Value + "\n")
	}
	return b.String()
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

var slugRepl = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "é", "e", "è", "e", "á", "a", "à", "a", "ó", "o", "ò", "o", "í", "i", "ñ", "n", "ç", "c")

// Slug turns a display name into a container-safe id.
func Slug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(slugRepl.Replace(strings.ToLower(name)), "-"), "-")
	if len(s) > 24 {
		s = strings.TrimRight(s[:24], "-")
	}
	return s
}

// NormalizeNames trims, dedupes and sorts player names.
func NormalizeNames(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

var playerName = regexp.MustCompile(`^[A-Za-z0-9_]{2,16}$`)

func ValidPlayerName(n string) bool { return playerName.MatchString(n) }
