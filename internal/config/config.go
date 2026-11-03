// Package config stores the global settings in <data>/cubeherd.json.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"cubeherd/internal/s3"
)

type Settings struct {
	PasswordHash     string `json:"passwordHash"`
	SessionSecret    string `json:"sessionSecret"`
	SessionDays      int    `json:"sessionDays"`
	CFAPIKey         string `json:"cfApiKey"`
	DefaultMemoryGB  int    `json:"defaultMemoryGb"`
	PortMin          int    `json:"portMin"`
	PortMax          int    `json:"portMax"`
	DefaultAutopause bool   `json:"defaultAutopause"`
	Timezone         string `json:"timezone,omitempty"` // IANA name; "" = TZ env / system
	Language         string `json:"language,omitempty"` // de | en; "" = German (installs before i18n)
	// BindIPs are the host addresses a server's port may be published on; "" (all interfaces) is always available.
	BindIPs       []string `json:"bindIps,omitempty"`
	DefaultBindIP string   `json:"defaultBindIp,omitempty"` // preselected in the wizard; "" = all interfaces
	// NoUpdateCheck turns off the GitHub release lookup for new Cubeherd versions.
	NoUpdateCheck   bool   `json:"noUpdateCheck,omitempty"`
	DismissedUpdate string `json:"dismissedUpdate,omitempty"` // release tag whose banner was dismissed
	// Remote is the optional S3 bucket that backups are uploaded to (nil = local backups only).
	Remote *s3.Config `json:"remote,omitempty"`

	// Nodes are the remote hosts (agents) this dashboard manages, next to the embedded local one.
	Nodes []Node `json:"nodes,omitempty"`
	// LocalName is the display name of the embedded local node; "" = the host name.
	LocalName string `json:"localName,omitempty"`

	// Accounts turns on the multi-user mode (users, roles, audit log in cubeherd.db). PasswordHash then only
	// matters again if the mode is switched off.
	Accounts bool `json:"accounts,omitempty"`
	// AuditDays is how long audit entries are kept: 0 = the default of 365 days, -1 = forever.
	AuditDays int `json:"auditDays,omitempty"`
	// Metrics is the statistics recorder of this host (the embedded node, or an agent that the dashboard configures).
	Metrics Metrics `json:"metrics,omitempty"`
	// ScrapeTokenHash is the SHA-256 of the Prometheus scrape token; "" = the /metrics endpoint is off.
	ScrapeTokenHash string `json:"scrapeTokenHash,omitempty"`
	// PublicURL is the address users reach the dashboard at; single sign-on needs it for its callback.
	PublicURL string `json:"publicUrl,omitempty"`
}

// Node is one remote agent. The token is stored like the CurseForge key (file mode 0600).
// Metrics configures the statistics recorder. Zero retention values mean the defaults (48 h, 30 d, 365 d, 256 MB).
type Metrics struct {
	Enabled     bool `json:"enabled,omitempty"`
	Keep1mHours int  `json:"keep1mHours,omitempty"`
	Keep5mDays  int  `json:"keep5mDays,omitempty"`
	Keep1hDays  int  `json:"keep1hDays,omitempty"`
	MaxMB       int  `json:"maxMb,omitempty"`
}

type Node struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Addr        string `json:"addr"`        // https://host:port
	Token       string `json:"token"`       // bearer token of the agent
	Fingerprint string `json:"fingerprint"` // pinned SHA-256 of the agent's certificate
	// Metrics is the recorder setting the dashboard pushes to this agent; nil leaves the agent's own setting alone.
	Metrics *Metrics `json:"metrics,omitempty"`
	// Per-node defaults, since host addresses and free ports differ between machines.
	PortMin       int      `json:"portMin,omitempty"`
	PortMax       int      `json:"portMax,omitempty"`
	BindIPs       []string `json:"bindIps,omitempty"`
	DefaultBindIP string   `json:"defaultBindIp,omitempty"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	s    Settings
}

func Load(dataDir string) (*Store, error) {
	st := &Store{path: filepath.Join(dataDir, "cubeherd.json")}
	b, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.s = Settings{SessionDays: 7, DefaultMemoryGB: 4, PortMin: 25565, PortMax: 25600, DefaultAutopause: true}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &st.s); err != nil {
			return nil, err
		}
	}
	if st.s.SessionSecret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		st.s.SessionSecret = hex.EncodeToString(buf)
		if err := st.save(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// Get returns a copy of the settings. The lists are copied too: Update changes them in place (a node's port range,
// say), and a caller must be able to read what it got without holding the lock.
func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s := st.s
	s.BindIPs = slices.Clone(s.BindIPs)
	if s.Nodes != nil {
		s.Nodes = make([]Node, len(st.s.Nodes))
		for i, n := range st.s.Nodes {
			n.BindIPs = slices.Clone(n.BindIPs)
			s.Nodes[i] = n
		}
	}
	return s
}

// Update applies fn to a copy and persists it.
func (st *Store) Update(fn func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	old := st.s
	fn(&st.s)
	if err := st.save(); err != nil {
		st.s = old
		return err
	}
	return nil
}

func (st *Store) save() error {
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
