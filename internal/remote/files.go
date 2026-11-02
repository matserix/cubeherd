package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/manager"
	"cubeherd/internal/node"
	"cubeherd/internal/server"
)

var (
	_ node.Files   = (*Client)(nil)
	_ node.Addons  = (*Client)(nil)
	_ node.Backups = (*Client)(nil)
	_ node.Node    = (*Client)(nil)
)

func srv(s *server.Server) string { return "/v1/servers/" + esc(s.ID) }
func q(name, v string) string     { return name + "=" + url.QueryEscape(v) }

// download copies a response body to w; a body that ends early is an error, not a short file.
func (c *Client) download(ctx context.Context, path string, w io.Writer) error {
	resp, err := c.raw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, resp.Body)
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// upload streams r as the request body. Only ctx ends it, so big uploads are not cut by the default timeout.
func (c *Client) upload(ctx context.Context, method, path string, r io.Reader, out any) error {
	resp, err := c.raw(ctx, method, path, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return decodeJSON(resp.Body, out)
}

// ---- Files ----

func (c *Client) Paths(s *server.Server) api.Paths {
	var p api.Paths
	_ = c.quick(http.MethodPost, "/v1/paths", s, &p)
	return p
}

// ArchiveDir is where the agent archives uninstalled servers.
func (c *Client) ArchiveDir() string { return c.cachedInfo().ArchiveDir }

func (c *Client) List(ctx context.Context, s *server.Server, rel string) ([]api.Entry, bool, error) {
	var l api.Listing
	err := c.send(ctx, http.MethodGet, srv(s)+"/files?"+q("path", rel), nil, &l)
	return l.Entries, l.Truncated, err
}

func (c *Client) Stat(ctx context.Context, s *server.Server, rel string) (api.Entry, error) {
	var e api.Entry
	return e, c.send(ctx, http.MethodGet, srv(s)+"/files/stat?"+q("path", rel), nil, &e)
}

func (c *Client) ReadText(ctx context.Context, s *server.Server, rel string) (string, bool, error) {
	var t api.Text
	err := c.send(ctx, http.MethodGet, srv(s)+"/files/text?"+q("path", rel), nil, &t)
	return t.Content, t.IsText, err
}

func (c *Client) Open(ctx context.Context, s *server.Server, rel string, w io.Writer) error {
	return c.download(ctx, srv(s)+"/files/content?"+q("path", rel), w)
}

func (c *Client) ReadDataFile(s *server.Server, name string) ([]byte, error) {
	ctx, cancel := withTimeout(c.Timeout)
	defer cancel()
	resp, err := c.raw(ctx, http.MethodGet, srv(s)+"/data?"+q("name", name), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func (c *Client) WriteFile(ctx context.Context, s *server.Server, rel string, r io.Reader) error {
	return c.upload(ctx, http.MethodPut, srv(s)+"/files/content?"+q("path", rel), r, nil)
}

func (c *Client) Remove(ctx context.Context, s *server.Server, rel string) error {
	return c.send(ctx, http.MethodDelete, srv(s)+"/files?"+q("path", rel), nil, nil)
}

func (c *Client) WorldZip(ctx context.Context, s *server.Server, w io.Writer) error {
	return c.download(ctx, srv(s)+"/world.zip", w)
}

// StageWorld streams the uploaded ZIP to the agent. The returned path is the agent's own and only informational.
func (c *Client) StageWorld(s *server.Server, r io.Reader) (string, error) {
	var out struct{ Path string }
	err := c.upload(context.Background(), http.MethodPut, srv(s)+"/world/stage", r, &out)
	return out.Path, err
}

// ImportWorld asks the agent to import the ZIP it has staged for this server; zipPath is ignored because
// the agent never accepts a path from the dashboard.
func (c *Client) ImportWorld(s *server.Server, _ string) error {
	return c.slow(http.MethodPost, srv(s)+"/world/import", nil, nil)
}

// ---- Addons ----

func (c *Client) AddonFiles(ctx context.Context, s *server.Server) ([]api.AddonFile, error) {
	var l []api.AddonFile
	return l, c.send(ctx, http.MethodGet, srv(s)+"/addons", nil, &l)
}

func (c *Client) AddonsPending(ctx context.Context, s *server.Server) bool {
	var r struct{ Pending bool }
	return c.send(ctx, http.MethodGet, srv(s)+"/addons/pending", nil, &r) == nil && r.Pending
}

// UploadAddon and DeleteUpload change the list of uploaded files in the definition; *s is updated to match.
func (c *Client) UploadAddon(ctx context.Context, s *server.Server, name string, r io.Reader) error {
	var got server.Server
	if err := c.upload(ctx, http.MethodPut, srv(s)+"/addons/"+esc(name), r, &got); err != nil {
		return err
	}
	*s = got
	return nil
}

func (c *Client) DeleteUpload(ctx context.Context, s *server.Server, name string) error {
	var got server.Server
	if err := c.send(ctx, http.MethodDelete, srv(s)+"/addons/"+esc(name), nil, &got); err != nil {
		return err
	}
	*s = got
	return nil
}

// ---- Backups ----

func (c *Client) BackupsAll(ctx context.Context, s *server.Server) ([]manager.Backup, error) {
	var l []api.Backup
	if err := c.send(ctx, http.MethodGet, srv(s)+"/backups", nil, &l); err != nil {
		return nil, err
	}
	out := make([]manager.Backup, len(l))
	for i, b := range l {
		out[i] = manager.Backup{Name: b.Name, Time: b.Time, Reason: b.Reason, Size: b.Size, Local: b.Local, Remote: b.Remote, Check: b.Check, CheckErr: b.CheckErr}
		if b.UploadErr != "" {
			out[i].UploadErr = errors.New(b.UploadErr)
		}
	}
	return out, nil
}

func (c *Client) BackupNow(id, reason string) error {
	return c.quick(http.MethodPost, "/v1/servers/"+esc(id)+"/backups?"+q("reason", reason), nil, nil)
}

func (c *Client) BackupHealth(s *server.Server, running bool) api.Health {
	var h api.Health
	if c.quick(http.MethodGet, srv(s)+"/backups/health?"+flag("running", running), nil, &h) != nil {
		return api.Health{Level: "off"}
	}
	return h
}

func (c *Client) backupAction(id, name, action string) error {
	return c.quick(http.MethodPost, "/v1/servers/"+esc(id)+"/backups/"+esc(name)+"/"+action, nil, nil)
}

func (c *Client) VerifyBackup(id, name string) error { return c.backupAction(id, name, "verify") }
func (c *Client) UploadBackup(id, name string) error { return c.backupAction(id, name, "upload") }
func (c *Client) Restore(id, name string) error      { return c.backupAction(id, name, "restore") }

func (c *Client) DeleteBackupEverywhere(ctx context.Context, s *server.Server, name string) error {
	return c.send(ctx, http.MethodDelete, srv(s)+"/backups/"+esc(name), nil, nil)
}

// OpenBackup streams an archive from the agent. The body is not seekable, so the dashboard serves it without ranges.
func (c *Client) OpenBackup(ctx context.Context, s *server.Server, name string) (io.ReadCloser, int64, time.Time, error) {
	resp, err := c.raw(ctx, http.MethodGet, srv(s)+"/backups/"+esc(name)+"/download", nil)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return resp.Body, resp.ContentLength, mod, nil
}

func (c *Client) HasRemote() bool {
	var r struct{ Configured bool }
	return c.quick(http.MethodGet, "/v1/backups/remote", nil, &r) == nil && r.Configured
}

func (c *Client) InvalidateRemote() {
	_ = c.quick(http.MethodPost, "/v1/backups/remote/invalidate", nil, nil)
}

func (c *Client) NextBackupFor(s *server.Server) time.Time {
	var r struct{ Next time.Time }
	_ = c.quick(http.MethodGet, srv(s)+"/backups/next", nil, &r)
	return r.Next
}

func (c *Client) WakeScheduler() { _ = c.quick(http.MethodPost, "/v1/scheduler/wake", nil, nil) }

// PushConfig hands the agent the global settings it needs for its own work.
func (c *Client) PushConfig(ctx context.Context, cfg api.Config) error {
	return c.send(ctx, http.MethodPut, "/v1/config", cfg, nil)
}
