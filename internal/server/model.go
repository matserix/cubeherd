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

// Addon is a mod or plugin that itzg installs on start (declarative, see Env).
type Addon struct {
	Source  string `json:"source"`            // modrinth | curseforge | spiget
	Ref     string `json:"ref"`               // Modrinth/CurseForge slug or Spigot resource id
	Name    string `json:"name"`              // display name
	Version string `json:"version,omitempty"` // optional pin (Modrinth version id/number, CurseForge file id)
	IconURL string `json:"iconUrl,omitempty"`
}

const (
	AddonModrinth   = "modrinth"
	AddonCurseForge = "curseforge"
	AddonSpiget     = "spiget"
)

var addonRef = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

// ValidAddon checks references before they end up in an environment variable.
func ValidAddon(a Addon) bool {
	switch a.Source {
	case AddonModrinth, AddonCurseForge:
	case AddonSpiget:
		if strings.Trim(a.Ref, "0123456789") != "" {
			return false
		}
	default:
		return false
	}
	return addonRef.MatchString(a.Ref) && (a.Version == "" || addonRef.MatchString(a.Version))
}

// AddonDir is the folder in /data that holds mods or plugins.
func (s *Server) AddonDir() string {
	if IsPluginType(s.Type) {
		return "plugins"
	}
	return "mods"
}

// SupportsAddons is false for Vanilla, which loads neither mods nor plugins.
func (s *Server) SupportsAddons() bool { return s.Type != "VANILLA" || s.Pack != nil }

// AddonEnv returns the itzg variables for declared mods/plugins (also used to detect pending changes).
func (s *Server) AddonEnv() []KV {
	var mr, cf, sp []string
	for _, a := range s.Addons {
		ref := a.Ref
		if a.Version != "" {
			ref += ":" + a.Version
		}
		switch a.Source {
		case AddonModrinth:
			mr = append(mr, ref)
		case AddonCurseForge:
			cf = append(cf, ref)
		case AddonSpiget:
			sp = append(sp, a.Ref)
		}
	}
	used := map[string]bool{}
	for _, src := range s.AddonSources {
		used[src] = true
	}
	var e []KV
	if len(mr) > 0 || used[AddonModrinth] {
		e = append(e, KV{"MODRINTH_PROJECTS", strings.Join(mr, ",")})
		if len(mr) > 0 {
			e = append(e, KV{"MODRINTH_DOWNLOAD_DEPENDENCIES", "required"})
		}
	}
	if len(cf) > 0 || used[AddonCurseForge] {
		e = append(e, KV{"CURSEFORGE_FILES", strings.Join(cf, ",")})
	}
	if len(sp) > 0 {
		e = append(e, KV{"SPIGET_RESOURCES", strings.Join(sp, ",")})
	}
	return e
}

// AddAddon appends an addon and remembers its source.
func (s *Server) AddAddon(a Addon) {
	s.Addons = append(s.Addons, a)
	for _, src := range s.AddonSources {
		if src == a.Source {
			return
		}
	}
	s.AddonSources = append(s.AddonSources, a.Source)
}

// BackupPlan is the per-server backup schedule.
type BackupPlan struct {
	Interval     string `json:"interval"` // off | 6h | daily | weekly
	Time         string `json:"time"`     // HH:MM, local time
	Keep         int    `json:"keep"`     // automatic backups to keep (manual ones are never pruned)
	BeforeUpdate bool   `json:"beforeUpdate"`
	OnlyIfActive bool   `json:"onlyIfActive"`         // skip scheduled runs while the server is stopped or paused
	Remote       bool   `json:"remote,omitempty"`     // also upload every backup to the external storage
	RemoteOnly   bool   `json:"remoteOnly,omitempty"` // delete the local copy after a successful upload (needs Remote)
	NoVerify     bool   `json:"noVerify,omitempty"`   // skip reading the archive back after creating it
	NoWarn       bool   `json:"noWarn,omitempty"`     // no health banner for missed or broken backups
	WarnAfter    string `json:"warnAfter,omitempty"`  // "" (2 missed runs) | 25h | 3d
}

// DefaultBackupPlan is used for new servers and servers created before backups existed.
func DefaultBackupPlan() BackupPlan {
	return BackupPlan{Interval: "daily", Time: "04:00", Keep: 7, BeforeUpdate: true, OnlyIfActive: true}
}

type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Server struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Type      string `json:"type"`    // itzg TYPE for software servers, loader for packs
	Version   string `json:"version"` // Minecraft version
	Pack      *Pack  `json:"pack,omitempty"`
	JavaTag   string `json:"javaTag"`
	Port      int    `json:"port"`
	MemoryGB  int    `json:"memoryGb"`
	Autopause bool   `json:"autopause"`
	Aikar     bool   `json:"aikar"`
	Autostart bool   `json:"autostart"`
	// Autostop stops the container after AutostopMinutes without players (itzg ENABLE_AUTOSTOP).
	// It excludes Autopause and forces restart policy "no".
	Autostop        bool        `json:"autostop,omitempty"`
	AutostopMinutes int         `json:"autostopMinutes,omitempty"`
	CPUs            float64     `json:"cpus,omitempty"` // container CPU limit; 0 = unlimited
	Props           Props       `json:"props"`
	Whitelist       []string    `json:"whitelist"`
	Ops             []string    `json:"ops"`
	ExtraEnv        []KV        `json:"extraEnv"`
	Backup          *BackupPlan `json:"backup,omitempty"`
	Addons          []Addon     `json:"addons,omitempty"`
	// AddonSources remembers sources used before: an explicitly empty list makes itzg remove
	// files it installed earlier, while an unset variable leaves them in place.
	AddonSources []string  `json:"addonSources,omitempty"`
	Uploads      []string  `json:"uploads,omitempty"`    // JAR files uploaded into the mods/plugins folder
	DataDir      string    `json:"dataDir,omitempty"`    // imported servers whose data lives in another host folder
	DataVol      string    `json:"dataVolume,omitempty"` // imported servers whose data lives in a Docker volume
	Imported     bool      `json:"imported,omitempty"`
	EULAPending  bool      `json:"eulaPending,omitempty"` // adopted container never accepted the EULA; Cubeherd must not accept it for the user
	CreatedAt    time.Time `json:"createdAt"`
}

func (s *Server) Container() string { return "mc-" + s.ID }

// Plan returns the backup plan, falling back to the defaults.
func (s *Server) Plan() BackupPlan {
	if s.Backup == nil {
		return DefaultBackupPlan()
	}
	return *s.Backup
}

// IdleMinutes is the autostop delay (default 30 minutes).
func (s *Server) IdleMinutes() int {
	if s.AutostopMinutes <= 0 {
		return 30
	}
	return s.AutostopMinutes
}

// IdleMode is "stop", "pause" or "none".
func (s *Server) IdleMode() string {
	switch {
	case s.Autostop:
		return "stop"
	case s.Autopause:
		return "pause"
	}
	return "none"
}

// RestartPolicy is the compose restart value; autostop requires "no".
func (s *Server) RestartPolicy() string {
	if s.Autostart && !s.Autostop {
		return "unless-stopped"
	}
	return "no"
}

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

// Globals are dashboard-wide settings that end up in every server's environment.
type Globals struct {
	CFKey string // CurseForge API key
	TZ    string // IANA time zone for log timestamps; "" keeps the image default
}

// Env builds the container environment in a stable order.
func (s *Server) Env(g Globals) []KV {
	cfKey := g.CFKey
	e := []KV{{"EULA", "TRUE"}}
	if s.EULAPending {
		e[0].Value = "FALSE"
	}
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
	if g.TZ != "" {
		e = append(e, KV{"TZ", g.TZ})
	}
	if s.Aikar {
		e = append(e, KV{"USE_AIKAR_FLAGS", "TRUE"})
	}
	if s.Autostop {
		secs := fmt.Sprint(s.IdleMinutes() * 60)
		e = append(e, KV{"ENABLE_AUTOSTOP", "TRUE"}, KV{"AUTOSTOP_TIMEOUT_EST", secs}, KV{"AUTOSTOP_TIMEOUT_INIT", secs})
	} else if s.Autopause {
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
	// The env lists are authoritative: entries removed in Cubeherd disappear on the next start.
	if len(s.Whitelist) > 0 {
		add("WHITELIST", strings.Join(s.Whitelist, ","))
		add("EXISTING_WHITELIST_FILE", "SYNCHRONIZE")
	}
	if len(s.Ops) > 0 {
		add("OPS", strings.Join(s.Ops, ","))
		add("EXISTING_OPS_FILE", "SYNCHRONIZE")
	}
	e = append(e, s.AddonEnv()...)
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
			return nil, &EnvLineError{Line: i + 1}
		}
		out = append(out, KV{k, v})
	}
	return out, nil
}

// EnvLineError reports a malformed line in the extra environment.
type EnvLineError struct{ Line int }

func (e *EnvLineError) Error() string { return fmt.Sprintf("Zeile %d: erwartet KEY=WERT", e.Line) }

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
