// Package config stores the global settings in <data>/cubeherd.json.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
}

// Node is one remote agent. The token is stored like the CurseForge key (file mode 0600).
type Node struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Addr        string `json:"addr"`        // https://host:port
	Token       string `json:"token"`       // bearer token of the agent
	Fingerprint string `json:"fingerprint"` // pinned SHA-256 of the agent's certificate
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

func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s
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
