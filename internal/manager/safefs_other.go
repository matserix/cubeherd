//go:build !unix

package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cubeherd/internal/server"
)

// Fallback for a platform without the file-descriptor-relative syscalls safefs_unix.go uses (for example
// Windows, which Cubeherd does not otherwise target: see host_other.go). It keeps the simpler check-then-open
// behaviour the rest of the manager had before, without the same protection against a symlink planted between
// the check and the open.

func openDirAt(root, rel string) (*os.File, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if rp != rootReal && !strings.HasPrefix(rp, rootReal+string(filepath.Separator)) {
		return nil, ErrOutside
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.IsDir() {
		f.Close()
		return nil, ErrOutside
	}
	return f, nil
}

func (m *Manager) hostLeaf(s *server.Server, rel string) (dir *os.File, leaf string, err error) {
	root := m.Store.DataDir(s)
	if root == "" {
		return nil, "", errors.New("no host folder")
	}
	if rel == "" {
		return nil, "", ErrOutside
	}
	dirRel, leaf := splitDirLeaf(rel)
	dir, err = openDirAt(root, dirRel)
	if err != nil {
		return nil, "", err
	}
	return dir, leaf, nil
}

func openLeafAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir.Name(), name), flags, os.FileMode(mode))
}

func renameAt(dir *os.File, oldName, newName string) error {
	base := dir.Name()
	return os.Rename(filepath.Join(base, oldName), filepath.Join(base, newName))
}

func removeAt(dir *os.File, name string) error {
	return os.RemoveAll(filepath.Join(dir.Name(), name))
}

func statLeaf(dir *os.File, name string) (size int64, modTime time.Time, isDir bool, err error) {
	fi, err := os.Lstat(filepath.Join(dir.Name(), name))
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return fi.Size(), fi.ModTime(), fi.IsDir(), nil
}
