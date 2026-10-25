package manager

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"cubeherd/internal/server"
)

// Entry is one item in a directory of /data.
type Entry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

const (
	MaxEditSize = 1 << 20 // larger files are offered for download only
	maxEntries  = 1000
)

var ErrOutside = errors.New("Pfad liegt außerhalb von /data")

// CleanRel normalises a path relative to /data ("" is the root).
func CleanRel(rel string) (string, error) {
	c := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	if strings.Contains(c, "\x00") {
		return "", ErrOutside
	}
	return strings.TrimPrefix(c, "/"), nil
}

var errNotRegular = errors.New("keine reguläre Datei")

// openRegular opens p for reading without blocking on FIFOs and refuses everything but regular files,
// since the server (a container the admin does not fully control) can plant FIFOs and devices in /data.
func openRegular(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

// limitWriter stops a copy once more than n bytes arrive and calls cancel to end the producer.
type limitWriter struct {
	w        io.Writer
	n        int64
	cancel   context.CancelFunc
	exceeded bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		l.exceeded = true
		l.cancel()
		return 0, errors.New("zu groß")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// hostPath resolves rel inside the server's host data folder, refusing symlinks that leave it.
func (m *Manager) hostPath(s *server.Server, rel string) (string, error) {
	root := m.Store.DataDir(s)
	if root == "" {
		return "", errors.New("no host folder")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(rootReal, filepath.FromSlash(rel))
	// Resolve the deepest existing ancestor and make sure it stays inside the root.
	check := p
	for {
		if real, err := filepath.EvalSymlinks(check); err == nil {
			if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
				return "", ErrOutside
			}
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	return p, nil
}

// dataShell runs a shell script with /data mounted: inside the running container or a throwaway one.
func (m *Manager) dataShell(ctx context.Context, s *server.Server, script string, env map[string]string, stdin io.Reader, stdout io.Writer, readOnly bool) error {
	var args []string
	running := false
	if c := m.inspect(ctx, s); c != nil && c.State.Running {
		running = true
	}
	if running {
		args = []string{"exec", "-i"}
	} else {
		mount := m.dataMount(s) + ":/data"
		if readOnly {
			mount += ":ro"
		}
		args = []string{"run", "--rm", "-i", "-v", mount, "--entrypoint", "sh"}
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	if running {
		args = append(args, s.Container(), "sh")
	} else {
		args = append(args, s.ImageRef())
	}
	args = append(args, "-c", script)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s", firstLine(stderr.String(), err))
	}
	return nil
}

// List returns the entries of a directory, folders first.
func (m *Manager) List(ctx context.Context, s *server.Server, rel string) ([]Entry, bool, error) {
	var out []Entry
	if m.Store.DataDir(s) != "" {
		p, err := m.hostPath(s, rel)
		if err != nil {
			return nil, false, err
		}
		des, err := os.ReadDir(p)
		if err != nil {
			return nil, false, err
		}
		for _, d := range des {
			e := Entry{Name: d.Name(), Dir: d.IsDir()}
			if info, err := d.Info(); err == nil {
				e.Size, e.ModTime = info.Size(), info.ModTime()
				if info.Mode()&os.ModeSymlink != 0 {
					if st, err := os.Stat(filepath.Join(p, d.Name())); err == nil {
						e.Dir = st.IsDir()
					}
				}
			}
			out = append(out, e)
		}
	} else {
		var buf bytes.Buffer
		script := `cd "/data/$P" && find . -mindepth 1 -maxdepth 1 -printf '%y %s %T@ %f\n'`
		if err := m.dataShell(ctx, s, script, map[string]string{"P": rel}, nil, &buf, true); err != nil {
			return nil, false, err
		}
		sc := bufio.NewScanner(&buf)
		for sc.Scan() {
			f := strings.SplitN(sc.Text(), " ", 4)
			if len(f) != 4 {
				continue
			}
			size, _ := strconv.ParseInt(f[1], 10, 64)
			sec, _ := strconv.ParseFloat(f[2], 64)
			out = append(out, Entry{Name: f[3], Dir: f[0] == "d", Size: size, ModTime: time.Unix(int64(sec), 0)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	truncated := len(out) > maxEntries
	if truncated {
		out = out[:maxEntries]
	}
	return out, truncated, nil
}

// Stat returns size information for a single file.
func (m *Manager) Stat(ctx context.Context, s *server.Server, rel string) (Entry, error) {
	if m.Store.DataDir(s) != "" {
		p, err := m.hostPath(s, rel)
		if err != nil {
			return Entry{}, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return Entry{}, err
		}
		return Entry{Name: info.Name(), Dir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime()}, nil
	}
	var buf bytes.Buffer
	if err := m.dataShell(ctx, s, `stat -L -c '%F|%s|%Y' "/data/$P"`, map[string]string{"P": rel}, nil, &buf, true); err != nil {
		return Entry{}, os.ErrNotExist
	}
	f := strings.Split(strings.TrimSpace(buf.String()), "|")
	if len(f) != 3 {
		return Entry{}, os.ErrNotExist
	}
	size, _ := strconv.ParseInt(f[1], 10, 64)
	sec, _ := strconv.ParseInt(f[2], 10, 64)
	return Entry{Name: path.Base(rel), Dir: f[0] == "directory", Size: size, ModTime: time.Unix(sec, 0)}, nil
}

// Open streams a file to w.
func (m *Manager) Open(ctx context.Context, s *server.Server, rel string, w io.Writer) error {
	if m.Store.DataDir(s) != "" {
		p, err := m.hostPath(s, rel)
		if err != nil {
			return err
		}
		f, err := openRegular(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
	return m.dataShell(ctx, s, `cat "/data/$P"`, map[string]string{"P": rel}, nil, w, true)
}

// ReadText returns a file's content when it is small UTF-8 text; ok is false for binary or large files.
func (m *Manager) ReadText(ctx context.Context, s *server.Server, rel string) (string, bool, error) {
	st, err := m.Stat(ctx, s, rel)
	if err != nil {
		return "", false, err
	}
	if st.Dir || st.Size > MaxEditSize {
		return "", false, nil
	}
	// The size check above uses metadata; the read itself is capped too, because a symlink, FIFO or growing
	// file planted by the container can report a small size and still stream without end.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, n: MaxEditSize, cancel: cancel}
	if err := m.Open(rctx, s, rel, lw); err != nil {
		if lw.exceeded {
			return "", false, nil
		}
		return "", false, err
	}
	b := buf.Bytes()
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return "", false, nil
	}
	return string(b), true, nil
}

// WriteFile replaces or creates a file inside /data.
func (m *Manager) WriteFile(ctx context.Context, s *server.Server, rel string, r io.Reader) error {
	if rel == "" {
		return ErrOutside
	}
	if m.Store.DataDir(s) != "" {
		if _, err := m.hostPath(s, rel); err != nil {
			return err
		}
	}
	return m.writeDataFile(ctx, s, rel, r)
}

// Remove deletes a file or folder inside /data (never /data itself).
func (m *Manager) Remove(ctx context.Context, s *server.Server, rel string) error {
	if rel == "" {
		return ErrOutside
	}
	if m.Store.DataDir(s) != "" {
		p, err := m.hostPath(s, rel)
		if err != nil {
			return err
		}
		return os.RemoveAll(p)
	}
	return m.dataShell(ctx, s, `rm -rf -- "/data/$P"`, map[string]string{"P": rel}, nil, io.Discard, false)
}
