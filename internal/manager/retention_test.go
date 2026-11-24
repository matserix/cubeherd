package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Restoring the oldest automatic backup makes a safety copy, which must not push that very archive out.
func TestPruneKeepsPinnedBackup(t *testing.T) {
	plan := daily()
	plan.Keep = 1
	m, s := healthFixture(t, plan, time.Now())
	if err := os.MkdirAll(m.BackupDir(s), 0o755); err != nil {
		t.Fatal(err)
	}
	const selected, other, safety = "20260101-040000_schedule.tar.zst", "20260102-040000_schedule.tar.zst", "20260103-040000_restore.tar.zst"
	for i, n := range []string{selected, other, safety} {
		p := filepath.Join(m.BackupDir(s), n)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if n := m.prune(s, selected); n != 1 {
		t.Errorf("pruned %d archives, want 1", n)
	}
	for name, want := range map[string]bool{selected: true, other: false, safety: true} {
		if _, err := m.BackupPath(s, name); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
}

func TestRemotePruneKeepsPinnedBackup(t *testing.T) {
	m, s, fb := remoteFixture(t) // Keep: 1
	old := time.Now().Add(-72 * time.Hour)
	const selected, other = "20260101-040000_schedule.tar.zst", "20260102-040000_schedule.tar.zst"
	fb.put("cubeherd/srv/"+selected, "a", old)
	fb.put("cubeherd/srv/"+other, "b", old.Add(time.Hour))
	if err := os.MkdirAll(m.BackupDir(s), 0o755); err != nil {
		t.Fatal(err)
	}
	const safety = "20260105-040000_restore.tar.zst"
	if err := os.WriteFile(filepath.Join(m.BackupDir(s), safety), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	op := testOp()
	op.pinned = selected
	if err := m.upload(context.Background(), s, op, safety); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if _, ok := fb.objs["cubeherd/srv/"+selected]; !ok {
		t.Error("the archive being restored was deleted")
	}
	if _, ok := fb.objs["cubeherd/srv/"+other]; ok {
		t.Error("an old archive was not pruned")
	}
}
