package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store keeps one folder per server under <root>/servers/<id>.
type Store struct {
	root string
	mu   sync.RWMutex
}

var ErrNotFound = errors.New("Server nicht gefunden")

func NewStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "servers"), 0o755); err != nil {
		return nil, err
	}
	return &Store{root: abs}, nil
}

func (st *Store) Root() string             { return st.root }
func (st *Store) Dir(id string) string     { return filepath.Join(st.root, "servers", id) }
func (st *Store) defPath(id string) string { return filepath.Join(st.Dir(id), "forgecraft.json") }

// DataDir is the host path mounted at /data ("" when the data lives in a Docker volume).
func (st *Store) DataDir(s *Server) string {
	if s.DataVol != "" {
		return ""
	}
	if s.DataDir != "" {
		return s.DataDir
	}
	return filepath.Join(st.Dir(s.ID), "data")
}

func (st *Store) List() ([]*Server, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	entries, err := os.ReadDir(filepath.Join(st.root, "servers"))
	if err != nil {
		return nil, err
	}
	var out []*Server
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := st.read(e.Name())
		if err != nil {
			continue // half-written or foreign folder
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (st *Store) Get(id string) (*Server, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.read(id)
}

func (st *Store) read(id string) (*Server, error) {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(st.defPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var s Server
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	return &s, nil
}

func (st *Store) Exists(id string) bool {
	_, err := os.Stat(st.Dir(id))
	return err == nil
}

// Save writes the definition and regenerates compose.yml.
func (st *Store) Save(s *Server, g Globals) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	dir := st.Dir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if s.DataDir == "" && s.DataVol == "" {
		if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(st.defPath(s.ID), b, 0o600); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "compose.yml"), []byte(Compose(s, g)), 0o600)
}

// RemoveDir deletes the server folder. It fails with a permission error when the
// container wrote files as another user; callers fall back to removing via Docker.
func (st *Store) RemoveDir(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return ErrNotFound
	}
	return os.RemoveAll(st.Dir(id))
}

// RemoveDefinition keeps data but deletes the definition, so the folder can be imported again.
func (st *Store) RemoveDefinition(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.Remove(st.defPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(st.Dir(id), st.Dir(id)+".removed")
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
