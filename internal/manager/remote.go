package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cubeherd/internal/s3"
	"cubeherd/internal/server"
)

// remoteTTL bounds how often the external storage is listed for the backup tab.
const remoteTTL = time.Minute

type remoteEntry struct {
	at   time.Time
	list []s3.Object
}

// remote returns the S3 client, or nil when no external storage is configured.
func (m *Manager) remote() *s3.Client {
	cfg := m.Config.Get().Remote
	if cfg == nil || !cfg.Valid() {
		return nil
	}
	c, err := s3.New(*cfg)
	if err != nil {
		return nil
	}
	return c
}

// HasRemote reports whether an external storage is configured.
func (m *Manager) HasRemote() bool { return m.remote() != nil }

// InvalidateRemote drops cached listings (after settings or uploads/deletes changed).
func (m *Manager) InvalidateRemote() {
	m.mu.Lock()
	m.rcache = map[string]remoteEntry{}
	m.mu.Unlock()
}

func (m *Manager) remotePrefix(c *s3.Client, s *server.Server) string { return c.Key(s.ID) + "/" }

// remoteObjects lists a server's archives in the external storage, cached for a minute.
func (m *Manager) remoteObjects(ctx context.Context, s *server.Server, fresh bool) ([]s3.Object, error) {
	c := m.remote()
	if c == nil {
		return nil, nil
	}
	m.mu.Lock()
	e, ok := m.rcache[s.ID]
	m.mu.Unlock()
	if ok && !fresh && time.Since(e.at) < remoteTTL {
		return e.list, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	list, err := c.List(ctx, m.remotePrefix(c, s))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.rcache[s.ID] = remoteEntry{at: time.Now(), list: list}
	m.mu.Unlock()
	return list, nil
}

func (m *Manager) hasRemoteBackup(s *server.Server, name string) bool {
	c := m.remote()
	if c == nil {
		return false
	}
	list, err := m.remoteObjects(context.Background(), s, false)
	if err != nil {
		return false
	}
	key := c.Key(s.ID, name)
	for _, o := range list {
		if o.Key == key {
			return true
		}
	}
	return false
}

// BackupsAll lists local and external archives together, newest first. A listing error of the
// external storage is returned next to the local archives, so the tab stays usable.
func (m *Manager) BackupsAll(ctx context.Context, s *server.Server) ([]Backup, error) {
	local, err := m.Backups(s)
	if err != nil {
		return nil, err
	}
	c := m.remote()
	if c == nil {
		m.annotate(s, local)
		return local, nil
	}
	objs, rerr := m.remoteObjects(ctx, s, false)
	byName := map[string]*Backup{}
	for i := range local {
		byName[local[i].Name] = &local[i]
	}
	var remoteOnly []Backup // appended after the loop: growing local would invalidate byName's pointers
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, m.remotePrefix(c, s))
		mm := backupName.FindStringSubmatch(name)
		if mm == nil {
			continue
		}
		if b := byName[name]; b != nil {
			b.Remote = true
			continue
		}
		remoteOnly = append(remoteOnly, Backup{Name: name, Reason: mm[2], Time: o.Modified, Size: o.Size, Remote: true})
	}
	local = append(local, remoteOnly...)
	sort.Slice(local, func(i, j int) bool { return local[i].Time.After(local[j].Time) })
	m.annotate(s, local)
	return local, rerr
}

// upload copies one local archive to the external storage and prunes old automatic archives there.
func (m *Manager) upload(ctx context.Context, s *server.Server, o *Op, name string) error {
	c := m.remote()
	if c == nil {
		return errors.New(m.t("Kein externer Speicher eingerichtet"))
	}
	p, err := m.BackupPath(s, name)
	if err != nil {
		return err
	}
	o.add("[cubeherd] " + m.t("Lade %s in den externen Speicher hoch", name))
	start := time.Now()
	if err := c.PutFile(ctx, c.Key(s.ID, name), p); err != nil {
		return err
	}
	m.InvalidateRemote()
	o.add("[cubeherd] " + m.t("Upload fertig (%s)", time.Since(start).Round(time.Second)))
	if n := m.pruneRemote(ctx, c, s); n > 0 {
		o.add("[cubeherd] " + m.t("%d alte automatische Backups im externen Speicher gelöscht", n))
	}
	return nil
}

// pruneRemote deletes automatic archives beyond the plan's Keep value in the external storage.
func (m *Manager) pruneRemote(ctx context.Context, c *s3.Client, s *server.Server) int {
	objs, err := m.remoteObjects(ctx, s, true)
	if err != nil {
		return 0
	}
	type item struct {
		key  string
		time time.Time
	}
	var auto []item
	for _, ob := range objs {
		mm := backupName.FindStringSubmatch(strings.TrimPrefix(ob.Key, m.remotePrefix(c, s)))
		if mm != nil && mm[2] != ReasonManual {
			auto = append(auto, item{ob.Key, ob.Modified})
		}
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].time.After(auto[j].time) })
	keep, n := max(s.Plan().Keep, 1), 0
	for i := keep; i < len(auto); i++ {
		if c.Delete(ctx, auto[i].key) == nil {
			n++
		}
	}
	if n > 0 {
		m.InvalidateRemote()
	}
	return n
}

// UploadBackup uploads an existing local archive in the background.
func (m *Manager) UploadBackup(id, name string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	if m.remote() == nil {
		return errors.New(m.t("Kein externer Speicher eingerichtet"))
	}
	if _, err := m.BackupPath(s, name); err != nil {
		return errors.New(m.t("Backup nicht gefunden"))
	}
	return m.run(id, server.StatusBackingUp, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		return m.upload(ctx, s, o, name)
	})
}

// fetch downloads an archive from the external storage into the local backup folder.
func (m *Manager) fetch(ctx context.Context, s *server.Server, o *Op, name string) error {
	c := m.remote()
	if c == nil || !backupName.MatchString(name) {
		return errors.New(m.t("Backup nicht gefunden"))
	}
	o.add("[cubeherd] " + m.t("Lade %s aus dem externen Speicher herunter", name))
	rc, _, err := c.Get(ctx, c.Key(s.ID, name))
	if err != nil {
		return err
	}
	defer rc.Close()
	dir := m.BackupDir(s)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".part")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// DeleteBackupEverywhere removes an archive from the local folder and the external storage.
func (m *Manager) DeleteBackupEverywhere(ctx context.Context, s *server.Server, name string) error {
	if !backupName.MatchString(name) {
		return os.ErrNotExist
	}
	var errs []error
	if _, err := m.BackupPath(s, name); err == nil {
		errs = append(errs, m.DeleteBackup(s, name))
	}
	if c := m.remote(); c != nil && m.hasRemoteBackup(s, name) {
		if err := c.Delete(ctx, c.Key(s.ID, name)); err != nil {
			errs = append(errs, err)
		}
		m.InvalidateRemote()
	}
	return errors.Join(errs...)
}

// OpenRemote streams an archive from the external storage (download of remote-only backups).
func (m *Manager) OpenRemote(ctx context.Context, s *server.Server, name string) (io.ReadCloser, int64, error) {
	c := m.remote()
	if c == nil || !backupName.MatchString(name) {
		return nil, 0, os.ErrNotExist
	}
	body, size, err := c.Get(ctx, c.Key(s.ID, name))
	if err != nil {
		if s3.IsNotFound(err) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	return body, size, nil
}

// CheckRemote verifies the credentials of a candidate configuration.
func CheckRemote(ctx context.Context, cfg s3.Config) error {
	c, err := s3.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return c.Check(ctx)
}
