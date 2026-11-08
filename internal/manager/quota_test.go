package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cubeherd/internal/api"
)

func TestQuotaLevels(t *testing.T) {
	for _, c := range []struct {
		used, limit int64
		want        string
	}{{0, 0, ""}, {10, 100, api.QuotaOK}, {89, 100, api.QuotaOK}, {90, 100, api.QuotaWarn}, {100, 100, api.QuotaFull}, {119, 100, api.QuotaFull}, {120, 100, api.QuotaOver}} {
		if got := quotaLevel(c.used, c.limit); got != c.want {
			t.Errorf("%d/%d = %q, want %q", c.used, c.limit, got, c.want)
		}
	}
}

func TestQuotaRefusesWritesWhenFullAndCapsUploads(t *testing.T) {
	old := quotaUnit
	quotaUnit = 1000
	t.Cleanup(func() { quotaUnit = old })
	m, s, dir := hostServer(t)
	s.DiskGB = 1 // 1000 bytes
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "world.dat"), make([]byte, 400), 0o600); err != nil {
		t.Fatal(err)
	}
	m.measureQuota(s)
	if q := m.QuotaOf(s); q.Level != api.QuotaOK || q.Used != 400 {
		t.Fatalf("%+v", q)
	}
	// an upload that would pass the limit is cut off instead of filling the disk
	err := m.WriteFile(ctx, s, "big.bin", bytes.NewReader(make([]byte, 5000)))
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("an oversized upload: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "big.bin")); err == nil && fi.Size() > 600 {
		t.Fatalf("%d bytes were written past the limit", fi.Size())
	}
	// below the limit it still works
	if err := m.WriteFile(ctx, s, "small.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("a small write below the limit: %v", err)
	}
	// once the folder is at the limit, writes and starts are refused
	_ = os.WriteFile(filepath.Join(dir, "more.dat"), make([]byte, 700), 0o600)
	m.measureQuota(s)
	if q := m.QuotaOf(s); q.Level != api.QuotaFull && q.Level != api.QuotaOver {
		t.Fatalf("%+v", q)
	}
	if err := m.WriteFile(ctx, s, "x.txt", strings.NewReader("x")); !errors.Is(err, ErrQuota) {
		t.Fatalf("write at the limit: %v", err)
	}
	// deleting is always allowed, and frees the room again
	if err := m.Remove(ctx, s, "more.dat"); err != nil {
		t.Fatalf("deleting at the limit: %v", err)
	}
	m.measureQuota(s)
	if err := m.WriteFile(ctx, s, "x.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("after freeing space: %v", err)
	}
	// no limit, no checks
	s.DiskGB = 0
	if m.QuotaOf(s) != nil {
		t.Fatal("quota reported without a limit")
	}
}

func TestSuspendedServerCannotStart(t *testing.T) {
	m, s, _ := hostServer(t)
	s.Suspended = true
	if err := m.mayStart(s); !errors.Is(err, ErrSuspended) {
		t.Fatalf("%v", err)
	}
	s.Suspended = false
	if err := m.mayStart(s); err != nil {
		t.Fatal(err)
	}
}
