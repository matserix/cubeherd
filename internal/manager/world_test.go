package manager

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) *zip.Reader {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, _ := zw.Create(n)
		io.WriteString(w, c)
	}
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func TestWorldRootAndZipToTar(t *testing.T) {
	zr := makeZip(t, map[string]string{
		"Meine Welt/level.dat":        "L",
		"Meine Welt/region/r.0.0.mca": "R",
		"Meine Welt/DIM-1/level.dat":  "N", // deeper level.dat must not win
		"__MACOSX/Meine Welt/._x":     "junk",
		"Meine Welt/../../etc/passwd": "evil",
	})
	root, err := WorldRoot(zr)
	if err != nil || root != "Meine Welt/" {
		t.Fatalf("root %q %v", root, err)
	}
	var buf bytes.Buffer
	if err := zipToTar(zr, root, "world", &buf); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	got := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got[h.Name] = true
	}
	for _, want := range []string{"world/level.dat", "world/region/r.0.0.mca", "world/DIM-1/level.dat"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for n := range got {
		if n == "world/etc/passwd" || n == "etc/passwd" {
			continue // cleaned into the world folder is acceptable, escaping is not
		}
		if len(n) < 6 || n[:6] != "world/" {
			t.Errorf("entry escaped target: %s", n)
		}
	}
	if _, err := WorldRoot(makeZip(t, map[string]string{"readme.txt": "x"})); err == nil {
		t.Error("zip without level.dat must be rejected")
	}
}

func TestCleanRel(t *testing.T) {
	for in, want := range map[string]string{"": "", "config/x.toml": "config/x.toml", "../../etc": "etc", "/a/../b": "b", `a\b`: "a/b"} {
		if got, _ := CleanRel(in); got != want {
			t.Errorf("CleanRel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTarToZip(t *testing.T) {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "./world/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "./world/level.dat", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.Close()
	var zb bytes.Buffer
	if err := tarToZip(&tb, &zb); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(zb.Bytes()), int64(zb.Len()))
	if len(zr.File) != 2 || zr.File[1].Name != "world/level.dat" {
		t.Errorf("zip entries: %v", zr.File)
	}
}

func TestProgressWriter(t *testing.T) {
	var got []int
	p := &progressWriter{w: io.Discard, total: 1000, report: func(pct int) { got = append(got, pct) }}
	for i := 0; i < 10; i++ {
		p.Write(make([]byte, 100))
	}
	want := []int{10, 20, 30, 40, 50, 60, 70, 80, 90}
	if len(got) != len(want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reports = %v, want %v", got, want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KB", 5 << 30: "5.0 GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
