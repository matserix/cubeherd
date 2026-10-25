//go:build unix

package manager

import (
	"io/fs"
	"syscall"
)

// ownerOf returns the numeric owner of a file, or -1/-1 when the platform does not report it.
func ownerOf(fi fs.FileInfo) (uid, gid int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}
