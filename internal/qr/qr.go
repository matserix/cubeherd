// Package qr draws QR codes (ISO 18004, byte mode, error correction level M, versions 1 to 10) with the standard
// library only. It is small on purpose: Cubeherd needs it for the authenticator setup page, so no service is involved.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// version describes one QR version at error correction level M.
type version struct {
	ec       int // error correction codewords per block
	g1n, g1d int // group 1: number of blocks and data codewords per block
	g2n, g2d int // group 2
	align    []int
}

var versions = [...]version{
	1:  {10, 1, 16, 0, 0, nil},
	2:  {16, 1, 28, 0, 0, []int{6, 18}},
	3:  {26, 1, 44, 0, 0, []int{6, 22}},
	4:  {18, 2, 32, 0, 0, []int{6, 26}},
	5:  {24, 2, 43, 0, 0, []int{6, 30}},
	6:  {16, 4, 27, 0, 0, []int{6, 34}},
	7:  {18, 4, 31, 0, 0, []int{6, 22, 38}},
	8:  {22, 2, 38, 2, 39, []int{6, 24, 42}},
	9:  {22, 3, 36, 2, 37, []int{6, 26, 46}},
	10: {26, 4, 43, 1, 44, []int{6, 28, 50}},
}

func (v version) dataWords() int { return v.g1n*v.g1d + v.g2n*v.g2d }

// ---------------- Reed-Solomon over GF(256), polynomial 0x11D ----------------

var exp [512]byte
var log [256]int

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		exp[i] = byte(x)
		log[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		exp[i] = exp[i-255]
	}
}

func mul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return exp[log[a]+log[b]]
}

func generator(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= mul(c, exp[i])
		}
		g = next
	}
	return g
}

func remainder(data []byte, n int) []byte {
	g := generator(n)
	rem := make([]byte, n)
	for _, d := range data {
		f := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for i := 0; i < n; i++ {
			rem[i] ^= mul(g[i+1], f)
		}
	}
	return rem
}

// ---------------- Encoding ----------------

type bits struct {
	b []bool
}

func (w *bits) add(v, n int) {
	for i := n - 1; i >= 0; i-- {
		w.b = append(w.b, v>>i&1 == 1)
	}
}

func codewords(text string, ver int, v version) ([]byte, error) {
	data := []byte(text)
	var w bits
	w.add(0b0100, 4)
	if ver <= 9 {
		w.add(len(data), 8)
	} else {
		w.add(len(data), 16)
	}
	for _, c := range data {
		w.add(int(c), 8)
	}
	capBits := v.dataWords() * 8
	if len(w.b) > capBits {
		return nil, errors.New("too long")
	}
	for t := 0; t < 4 && len(w.b) < capBits; t++ {
		w.b = append(w.b, false)
	}
	for len(w.b)%8 != 0 {
		w.b = append(w.b, false)
	}
	out := make([]byte, 0, v.dataWords())
	for i := 0; i < len(w.b); i += 8 {
		var c byte
		for j := 0; j < 8; j++ {
			c <<= 1
			if w.b[i+j] {
				c |= 1
			}
		}
		out = append(out, c)
	}
	for pad := byte(0xEC); len(out) < v.dataWords(); {
		out = append(out, pad)
		if pad == 0xEC {
			pad = 0x11
		} else {
			pad = 0xEC
		}
	}
	return out, nil
}

// interleave splits the data into blocks, adds error correction and interleaves them.
func interleave(data []byte, v version) []byte {
	var blocks, ecs [][]byte
	pos := 0
	for i := 0; i < v.g1n+v.g2n; i++ {
		n := v.g1d
		if i >= v.g1n {
			n = v.g2d
		}
		blk := data[pos : pos+n]
		pos += n
		blocks = append(blocks, blk)
		ecs = append(ecs, remainder(blk, v.ec))
	}
	var out []byte
	maxd := v.g1d
	if v.g2n > 0 {
		maxd = v.g2d
	}
	for i := 0; i < maxd; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < v.ec; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out
}

// ---------------- Matrix ----------------

type matrix struct {
	n        int
	dark     [][]bool
	reserved [][]bool
}

func newMatrix(n int) *matrix {
	m := &matrix{n: n}
	m.dark = make([][]bool, n)
	m.reserved = make([][]bool, n)
	for i := range m.dark {
		m.dark[i] = make([]bool, n)
		m.reserved[i] = make([]bool, n)
	}
	return m
}

func (m *matrix) set(r, c int, dark bool) { m.dark[r][c], m.reserved[r][c] = dark, true }

func (m *matrix) finder(r, c int) {
	for dr := -1; dr <= 7; dr++ {
		for dc := -1; dc <= 7; dc++ {
			rr, cc := r+dr, c+dc
			if rr < 0 || cc < 0 || rr >= m.n || cc >= m.n {
				continue
			}
			on := dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6 && (dr == 0 || dr == 6 || dc == 0 || dc == 6 || (dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4))
			m.set(rr, cc, on)
		}
	}
}

func (m *matrix) alignment(r, c int) {
	for dr := -2; dr <= 2; dr++ {
		for dc := -2; dc <= 2; dc++ {
			m.set(r+dr, c+dc, dr == -2 || dr == 2 || dc == -2 || dc == 2 || (dr == 0 && dc == 0))
		}
	}
}

func bch(value, gen, genBits, valueBits int) int {
	v := value << (genBits - 1)
	for i := valueBits + genBits - 2; i >= genBits-1; i-- {
		if v>>i&1 == 1 {
			v ^= gen << (i - (genBits - 1))
		}
	}
	return value<<(genBits-1) | v
}

func (m *matrix) functionPatterns(ver int, v version) {
	n := m.n
	m.finder(0, 0)
	m.finder(0, n-7)
	m.finder(n-7, 0)
	for i := 8; i < n-8; i++ { // timing
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}
	for _, r := range v.align {
		for _, c := range v.align {
			if (r == 6 && c == 6) || (r == 6 && c == v.align[len(v.align)-1]) || (c == 6 && r == v.align[len(v.align)-1]) {
				continue
			}
			m.alignment(r, c)
		}
	}
	// reserve format information areas
	for i := 0; i < 9; i++ {
		if !m.reserved[8][i] {
			m.set(8, i, false)
		}
		if !m.reserved[i][8] {
			m.set(i, 8, false)
		}
	}
	for i := 0; i < 8; i++ {
		m.set(8, n-1-i, false)
		m.set(n-1-i, 8, false)
	}
	m.set(n-8, 8, true) // the dark module
	if ver >= 7 {
		info := bch(ver, 0x1F25, 13, 6)
		for i := 0; i < 18; i++ {
			bit := info>>i&1 == 1
			a, b := i/3, i%3
			m.set(a, n-11+b, bit)
			m.set(n-11+b, a, bit)
		}
	}
}

func (m *matrix) place(data []byte) {
	n := m.n
	idx := 0
	next := func() bool {
		if idx >= len(data)*8 {
			return false
		}
		b := data[idx/8]>>(7-idx%8)&1 == 1
		idx++
		return b
	}
	up := true
	for c := n - 1; c > 0; c -= 2 {
		if c == 6 {
			c--
		}
		for i := 0; i < n; i++ {
			r := i
			if up {
				r = n - 1 - i
			}
			for dc := 0; dc < 2; dc++ {
				cc := c - dc
				if !m.reserved[r][cc] {
					m.dark[r][cc] = next()
				}
			}
		}
		up = !up
	}
}

var masks = [8]func(r, c int) bool{
	func(r, c int) bool { return (r+c)%2 == 0 },
	func(r, c int) bool { return r%2 == 0 },
	func(r, c int) bool { return c%3 == 0 },
	func(r, c int) bool { return (r+c)%3 == 0 },
	func(r, c int) bool { return (r/2+c/3)%2 == 0 },
	func(r, c int) bool { return r*c%2+r*c%3 == 0 },
	func(r, c int) bool { return (r*c%2+r*c%3)%2 == 0 },
	func(r, c int) bool { return ((r+c)%2+r*c%3)%2 == 0 },
}

func (m *matrix) masked(mask int) [][]bool {
	out := make([][]bool, m.n)
	for r := range out {
		out[r] = make([]bool, m.n)
		for c := range out[r] {
			out[r][c] = m.dark[r][c]
			if !m.reserved[r][c] && masks[mask](r, c) {
				out[r][c] = !out[r][c]
			}
		}
	}
	// format information: level M is 00
	f := bch(0<<3|mask, 0x537, 11, 5) ^ 0x5412
	n := m.n
	for i := 0; i < 15; i++ {
		bit := f>>i&1 == 1
		// first copy around the top-left finder
		switch {
		case i < 6:
			out[i][8] = bit
		case i == 6:
			out[7][8] = bit
		case i == 7:
			out[8][8] = bit
		case i == 8:
			out[8][7] = bit
		default:
			out[8][14-i] = bit
		}
		// second copy
		if i < 8 {
			out[8][n-1-i] = bit
		} else {
			out[n-15+i][8] = bit
		}
	}
	out[n-8][8] = true
	return out
}

func penalty(g [][]bool) int {
	n := len(g)
	score := 0
	for _, byRow := range []bool{true, false} {
		for i := 0; i < n; i++ {
			run, prev := 0, false
			for j := 0; j < n; j++ {
				v := g[i][j]
				if !byRow {
					v = g[j][i]
				}
				if j > 0 && v == prev {
					run++
				} else {
					if run >= 5 {
						score += run - 2
					}
					run = 1
				}
				prev = v
			}
			if run >= 5 {
				score += run - 2
			}
		}
	}
	for r := 0; r < n-1; r++ {
		for c := 0; c < n-1; c++ {
			if g[r][c] == g[r][c+1] && g[r][c] == g[r+1][c] && g[r][c] == g[r+1][c+1] {
				score += 3
			}
		}
	}
	pat := []bool{true, false, true, true, true, false, true, false, false, false, false}
	rev := []bool{false, false, false, false, true, false, true, true, true, false, true}
	match := func(get func(k int) bool, p []bool) bool {
		for k := range p {
			if get(k) != p[k] {
				return false
			}
		}
		return true
	}
	for i := 0; i < n; i++ {
		for j := 0; j+11 <= n; j++ {
			if match(func(k int) bool { return g[i][j+k] }, pat) || match(func(k int) bool { return g[i][j+k] }, rev) {
				score += 40
			}
			if match(func(k int) bool { return g[j+k][i] }, pat) || match(func(k int) bool { return g[j+k][i] }, rev) {
				score += 40
			}
		}
	}
	dark := 0
	for r := range g {
		for _, v := range g[r] {
			if v {
				dark++
			}
		}
	}
	pct := dark * 100 / (n * n)
	d := pct - 50
	if d < 0 {
		d = -d
	}
	score += d / 5 * 10
	return score
}

// Encode returns the module matrix of a QR code for text (true = dark). The quiet zone is not included.
func Encode(text string) ([][]bool, error) {
	for ver := 1; ver < len(versions); ver++ {
		v := versions[ver]
		data, err := codewords(text, ver, v)
		if err != nil {
			continue
		}
		all := interleave(data, v)
		m := newMatrix(17 + 4*ver)
		m.functionPatterns(ver, v)
		m.place(all)
		best, bestScore := -1, 0
		var bestGrid [][]bool
		for mask := 0; mask < 8; mask++ {
			g := m.masked(mask)
			if s := penalty(g); best < 0 || s < bestScore {
				best, bestScore, bestGrid = mask, s, g
			}
		}
		return bestGrid, nil
	}
	return nil, fmt.Errorf("the text is too long for a QR code (%d bytes)", len(text))
}

// SVG draws the code as an inline SVG with a quiet zone of four modules. size is the width in CSS pixels.
func SVG(text string, size int) (string, error) {
	g, err := Encode(text)
	if err != nil {
		return "", err
	}
	n := len(g) + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, size, size, n, n)
	for r, row := range g {
		for c, v := range row {
			if v {
				fmt.Fprintf(&b, "M%d,%dh1v1h-1z", c+4, r+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
