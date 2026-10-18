// Package host reports host resources for the overview tiles.
package host

import (
	"context"
	"sync"
	"time"

	"cubeherd/internal/docker"
)

type Stats struct {
	Cores      int
	CPUPercent float64 // -1 when unknown
	MemUsed    uint64
	MemTotal   uint64
	DiskUsed   uint64
	DiskTotal  uint64
	DiskPath   string
	Source     string // "host" or "docker" (Docker Desktop VM)
}

// Reader keeps the previous CPU sample so consecutive calls yield a delta.
type Reader struct {
	Docker  *docker.Client
	DataDir string

	mu       sync.Mutex
	prevIdle uint64
	prevAll  uint64
	cache    Stats
	cachedAt time.Time
}

func (r *Reader) Read(ctx context.Context) Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.cachedAt) < 2*time.Second {
		return r.cache
	}
	s := Stats{CPUPercent: -1, DiskPath: r.DataDir}
	if !r.readProc(&s) && r.Docker != nil {
		if info, err := r.Docker.Info(ctx); err == nil {
			s.Cores, s.MemTotal, s.Source = info.NCPU, info.MemTotal, "docker"
		}
	}
	s.DiskUsed, s.DiskTotal = disk(r.DataDir)
	r.cache, r.cachedAt = s, time.Now()
	return s
}
