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
	// Remote is the optional S3 bucket that backups are uploaded to (nil = local backups only).
	Remote *s3.Config `json:"remote,omitempty"`
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
