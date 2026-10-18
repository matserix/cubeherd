package host

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func (r *Reader) readProc(s *Stats) bool {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return false
	}
	defer f.Close()
	var total, avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	s.MemTotal, s.MemUsed, s.Cores, s.Source = total, total-avail, runtime.NumCPU(), "host"

	if b, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(b), "\n")
		var all, idle uint64
		for i, fld := range strings.Fields(line)[1:] {
			v, _ := strconv.ParseUint(fld, 10, 64)
			all += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		if r.prevAll > 0 && all > r.prevAll {
			s.CPUPercent = 100 * (1 - float64(idle-r.prevIdle)/float64(all-r.prevAll))
		}
		r.prevAll, r.prevIdle = all, idle
	}
	return total > 0
}

func disk(path string) (used, total uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0
	}
	total = st.Blocks * uint64(st.Bsize)
	return total - st.Bfree*uint64(st.Bsize), total
}
