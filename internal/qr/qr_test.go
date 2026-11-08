package qr

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// payloads cover every version the encoder offers, including the sizes where blocks and alignment patterns change.
func payloads() []string {
	var out []string
	for _, n := range []int{1, 5, 14, 15, 26, 42, 60, 84, 106, 122, 152, 180, 213} {
		out = append(out, strings.Repeat("a", n))
	}
	out = append(out,
		"otpauth://totp/Cubeherd:anna?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Cubeherd",
		"otpauth://totp/Cubeherd%3Aanna%40example.com?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=Cubeherd&algorithm=SHA1&digits=6&period=30",
		"Ünïcödé ✓ 日本語")
	return out
}

func TestSizesGrowWithTheText(t *testing.T) {
	prev := 0
	for _, p := range payloads()[:13] {
		g, err := Encode(p)
		if err != nil {
			t.Fatalf("%d bytes: %v", len(p), err)
		}
		n := len(g)
		if n < prev || (n-17)%4 != 0 {
			t.Fatalf("%d bytes gave a %d-module code after %d", len(p), n, prev)
		}
		prev = n
	}
	if _, err := Encode(strings.Repeat("x", 300)); err == nil {
		t.Fatal("a text beyond version 10 must be refused")
	}
}

func TestFinderAndTimingPatterns(t *testing.T) {
	g, _ := Encode("hello")
	n := len(g)
	for _, o := range [][2]int{{0, 0}, {0, n - 7}, {n - 7, 0}} {
		for i := 0; i < 7; i++ {
			if !g[o[0]][o[1]+i] || !g[o[0]+6][o[1]+i] || !g[o[0]+i][o[1]] || !g[o[0]+i][o[1]+6] {
				t.Fatalf("finder at %v has no solid ring", o)
			}
		}
		if g[o[0]+1][o[1]+1] || !g[o[0]+3][o[1]+3] {
			t.Fatalf("finder at %v is wrong inside", o)
		}
	}
	for i := 8; i < n-8; i++ {
		if g[6][i] != (i%2 == 0) || g[i][6] != (i%2 == 0) {
			t.Fatalf("timing pattern broken at %d", i)
		}
	}
	if !g[n-8][8] {
		t.Fatal("the dark module is missing")
	}
}

func TestSVG(t *testing.T) {
	s, err := SVG("hello", 200)
	if err != nil || !strings.HasPrefix(s, "<svg") || !strings.Contains(s, `width="200"`) || strings.Contains(s, "<script") {
		t.Fatalf("%v %q", err, s[:40])
	}
}

// TestWriteForDecoding writes PNGs for an external decoder (the swift script in the design notes) when QR_OUT is set.
func TestWriteForDecoding(t *testing.T) {
	dir := os.Getenv("QR_OUT")
	if dir == "" {
		t.Skip("QR_OUT not set")
	}
	for i, p := range payloads() {
		g, err := Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		const scale, quiet = 8, 4
		n := (len(g) + 2*quiet) * scale
		img := image.NewGray(image.Rect(0, 0, n, n))
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				img.SetGray(x, y, color.Gray{255})
			}
		}
		for r, row := range g {
			for c, v := range row {
				if !v {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.SetGray((c+quiet)*scale+dx, (r+quiet)*scale+dy, color.Gray{0})
					}
				}
			}
		}
		f, _ := os.Create(filepath.Join(dir, strings.Join([]string{"q", string(rune('a' + i)), ".png"}, "")))
		png.Encode(f, img)
		f.Close()
		os.WriteFile(filepath.Join(dir, "q"+string(rune('a'+i))+".txt"), []byte(p), 0o644)
	}
}
