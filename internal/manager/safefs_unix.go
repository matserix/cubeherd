//go:build unix

package manager

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"cubeherd/internal/server"
)

// This file resolves a path inside a server's data folder one component at a time, by file descriptor,
// refusing to follow a symlink anywhere along the way (O_NOFOLLOW on every open). hostPath (files.go) used to
// check containment once with os.Stat/EvalSymlinks and hand back a path string for a later os.Open/os.Stat to
// reopen by name; the server's own container owns /data and can run between that check and the later open, so
// a symlink planted in the meantime was followed after all. Resolving by descriptor closes that window: each
// step opens relative to the file descriptor already held for its parent, never by reassembling and
// reopening a path string, so nothing already resolved can be swapped afterwards.

func splitRelParts(rel string) []string {
	if rel == "" {
		return nil
	}
	return strings.Split(rel, "/")
}

// mapWalkErr turns a raw errno from the walk into the two errors callers already handle: a missing
// intermediate component is "not found" (a normal, expected case while browsing), and a non-directory or a
// symlink standing where the walk needs a directory is treated the same as a path that leaves /data, since
// Cubeherd never follows a symlink here on purpose.
func mapWalkErr(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return os.ErrNotExist
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		return ErrOutside
	default:
		return err
	}
}

// openDirAt opens the directory at root/rel, walking down one component at a time with O_NOFOLLOW|O_DIRECTORY.
// root must already be a resolved, symlink-free path (the caller resolves the data folder itself once, with
// filepath.EvalSymlinks, the same as before; only the walk below it is newly race-safe).
func openDirAt(root, rel string) (*os.File, error) {
	fd, err := unix.Open(root, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapWalkErr(err)
	}
	cur := fd
	for _, part := range splitRelParts(rel) {
		nfd, err := unix.Openat(cur, part, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW, 0)
		_ = unix.Close(cur)
		if err != nil {
			return nil, mapWalkErr(err)
		}
		cur = nfd
	}
	return os.NewFile(uintptr(cur), path.Join(root, rel)), nil
}

// hostLeaf opens the directory that holds rel (its parent, walked race-safely by openDirAt) and returns it
// together with the leaf name to use inside it via *at syscalls. rel must not be "" (there is no leaf of the
// data folder itself); callers already refuse that case before reaching here.
func (m *Manager) hostLeaf(s *server.Server, rel string) (dir *os.File, leaf string, err error) {
	root := m.Store.DataDir(s)
	if root == "" {
		return nil, "", errors.New("no host folder")
	}
	if rel == "" {
		return nil, "", ErrOutside
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", err
	}
	dirRel, leaf := splitDirLeaf(rel)
	dir, err = openDirAt(rootReal, dirRel)
	if err != nil {
		return nil, "", err
	}
	return dir, leaf, nil
}

// openLeafAt opens name inside dir without following a symlink there, whatever flags the caller otherwise asks for.
func openLeafAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, mapWalkErr(err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// renameAt renames oldName to newName inside dir (a single directory, as every caller here uses it: a
// temporary file replacing the file it is named after).
func renameAt(dir *os.File, oldName, newName string) error {
	return unix.Renameat(int(dir.Fd()), oldName, int(dir.Fd()), newName)
}

// removeAt removes name inside dir: a symlink, device, FIFO or regular file is unlinked directly without
// ever being followed; a directory is emptied first — recursively, the same way, by descriptor, never by a
// path string — and then removed itself.
func removeAt(dir *os.File, name string) error {
	switch err := unix.Unlinkat(int(dir.Fd()), name, 0); {
	case err == nil, errors.Is(err, os.ErrNotExist):
		return nil
	case !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM):
		// EISDIR (Linux) or EPERM (BSD/Darwin) means "it's a directory"; anything else is a real failure.
		return mapWalkErr(err)
	}
	sub, err := openLeafAt(dir, name, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	names, err := sub.Readdirnames(-1)
	if err != nil {
		sub.Close()
		return err
	}
	for _, n := range names {
		if err := removeAt(sub, n); err != nil {
			sub.Close()
			return err
		}
	}
	sub.Close()
	return unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR)
}

// statLeaf reports size, modification time and whether name is a directory, by descriptor and without ever
// following a symlink there (AT_SYMLINK_NOFOLLOW) or blocking on it (fstatat never blocks, unlike opening a
// FIFO would).
func statLeaf(dir *os.File, name string) (size int64, modTime time.Time, isDir bool, err error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, time.Time{}, false, mapWalkErr(err)
	}
	sec, nsec := st.Mtim.Unix()
	return st.Size, time.Unix(sec, nsec), st.Mode&unix.S_IFMT == unix.S_IFDIR, nil
}
