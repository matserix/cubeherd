//go:build !linux

package host

import "syscall"

// On macOS (Docker Desktop) the containers run in a VM; figures come from Docker /info.
func (r *Reader) readProc(*Stats) bool { return false }

func disk(path string) (used, total uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0
	}
	total = st.Blocks * uint64(st.Bsize)
	return total - st.Bfree*uint64(st.Bsize), total
}
