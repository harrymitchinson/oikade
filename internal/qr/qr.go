// Package qr makes QR codes (ISO/IEC 18004) of the one kind Oikade draws: text in the alphanumeric
// mode, at error correction level M, in versions 1 to 6, which is up to 154 characters. A Matter
// onboarding payload, "MT:" and 19 characters, fits version 2. Terminal draws one where a phone's
// camera can scan it.
package qr

import (
	"fmt"
	"strings"
)

// alphanumeric is the alphanumeric mode's characters, in the order of their values.
const alphanumeric = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

// version is one QR version's layout at error correction level M (ISO/IEC 18004, table 9). Each of
// versions 1 to 6 has blocks of one size.
type version struct {
	blocks, data, ec int // blocks, and each one's data and error correction codewords
	remainder        int // bits after the codewords
}

var versions = [...]version{
	1: {1, 16, 10, 0},
	2: {1, 28, 16, 7},
	3: {1, 44, 26, 7},
	4: {2, 32, 18, 7},
	5: {2, 43, 24, 7},
	6: {4, 27, 16, 7},
}

// Code is a QR code's modules.
type Code struct {
	// Size is how many modules a side has.
	Size    int
	Version int
	Mask    int
	dark    []bool
}

// Dark reports whether the module at row and col is dark. Outside the code, it's light.
func (c *Code) Dark(row, col int) bool {
	if row < 0 || col < 0 || row >= c.Size || col >= c.Size {
		return false
	}
	return c.dark[row*c.Size+col]
}

// Encode makes the smallest QR code that holds text, which may use only the alphanumeric mode's
// characters: digits, capital letters, space and $%*+-./:.
func Encode(text string) (*Code, error) {
	for _, r := range text {
		if !strings.ContainsRune(alphanumeric, r) {
			return nil, fmt.Errorf("qr: %q isn't in the alphanumeric mode", r)
		}
	}
	// Mode, a 9-bit count, 11 bits for each pair of characters and 6 for one left over.
	bits := 4 + 9 + 11*(len(text)/2) + 6*(len(text)%2)
	for v := 1; v < len(versions); v++ {
		if bits <= versions[v].blocks*versions[v].data*8 && len(text) < 1<<9 {
			return encode(text, v), nil
		}
	}
	return nil, fmt.Errorf("qr: %d characters don't fit version %d", len(text), len(versions)-1)
}

func encode(text string, v int) *Code {
	codewords := interleave(dataCodewords(text, v), v)
	size := 17 + 4*v
	best := (*Code)(nil)
	bestPenalty := 0
	for mask := range 8 {
		m := newMatrix(size, v)
		m.place(codewords)
		m.applyMask(mask)
		m.format(mask)
		c := &Code{Size: size, Version: v, Mask: mask, dark: m.dark}
		if p := penalty(c); best == nil || p < bestPenalty {
			best, bestPenalty = c, p
		}
	}
	return best
}

// bitBuffer collects bits, most significant first.
type bitBuffer struct {
	bytes []byte
	n     int
}

func (b *bitBuffer) put(v, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if v>>i&1 != 0 {
			b.bytes[b.n/8] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

// dataCodewords is text's data codewords for version v: the alphanumeric segment, the terminator,
// and the padding that fills the version (ISO/IEC 18004, 7.4).
func dataCodewords(text string, v int) []byte {
	capacity := versions[v].blocks * versions[v].data
	var b bitBuffer
	b.put(0b0010, 4)
	b.put(len(text), 9)
	for i := 0; i+1 < len(text); i += 2 {
		b.put(45*strings.IndexByte(alphanumeric, text[i])+strings.IndexByte(alphanumeric, text[i+1]), 11)
	}
	if len(text)%2 == 1 {
		b.put(strings.IndexByte(alphanumeric, text[len(text)-1]), 6)
	}
	b.put(0, min(4, capacity*8-b.n))
	b.put(0, (8-b.n%8)%8)
	for pad := 0xEC; len(b.bytes) < capacity; pad ^= 0xEC ^ 0x11 {
		b.bytes = append(b.bytes, byte(pad))
	}
	return b.bytes
}

// interleave splits data into version v's blocks, adds each one's error correction, and interleaves
// them: each block's first data codeword, then each one's second, and so on, then the same for the
// error correction codewords (ISO/IEC 18004, 7.6).
func interleave(data []byte, v int) []byte {
	ver := versions[v]
	gen := generator(ver.ec)
	out := make([]byte, 0, ver.blocks*(ver.data+ver.ec))
	ec := make([][]byte, ver.blocks)
	for i := range ver.blocks {
		ec[i] = remainder(data[i*ver.data:(i+1)*ver.data], gen)
	}
	for j := range ver.data {
		for i := range ver.blocks {
			out = append(out, data[i*ver.data+j])
		}
	}
	for j := range ver.ec {
		for i := range ver.blocks {
			out = append(out, ec[i][j])
		}
	}
	return out
}

// GF(256) with the QR code's polynomial, x^8 + x^4 + x^3 + x^2 + 1.
var gfExp, gfLog = func() (exp [512]byte, log [256]byte) {
	x := 1
	for i := range 255 {
		exp[i] = byte(x)
		log[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < len(exp); i++ {
		exp[i] = exp[i-255]
	}
	return exp, log
}()

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// generator is the Reed-Solomon generator polynomial of degree n, (x - α^0)…(x - α^(n-1)), with its
// coefficients highest first and the leading 1 left out.
func generator(n int) []byte {
	g := []byte{1}
	for i := range n {
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= gfMul(c, gfExp[i])
		}
		g = next
	}
	return g[1:]
}

// remainder is the error correction codewords of data: the remainder of data·x^n divided by gen.
func remainder(data, gen []byte) []byte {
	r := make([]byte, len(gen))
	for _, d := range data {
		factor := d ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i, c := range gen {
			r[i] ^= gfMul(c, factor)
		}
	}
	return r
}

// matrix is a code being built: its modules, and which of them are function patterns, which data
// and masks leave alone.
type matrix struct {
	size     int
	dark     []bool
	function []bool
}

func newMatrix(size, v int) *matrix {
	m := &matrix{size: size, dark: make([]bool, size*size), function: make([]bool, size*size)}
	for _, at := range [][2]int{{0, 0}, {0, size - 7}, {size - 7, 0}} {
		m.finder(at[0], at[1])
	}
	for i := 8; i < size-8; i++ {
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}
	if v >= 2 {
		// Versions 2 to 6 have one alignment pattern, as the others would overlap the finders.
		c := size - 7
		for dr := -2; dr <= 2; dr++ {
			for dc := -2; dc <= 2; dc++ {
				m.set(c+dr, c+dc, max(abs(dr), abs(dc)) != 1)
			}
		}
	}
	m.format(0) // reserves the format information's modules, and sets the dark module
	return m
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// set sets a function pattern's module.
func (m *matrix) set(row, col int, dark bool) {
	m.dark[row*m.size+col] = dark
	m.function[row*m.size+col] = true
}

// finder draws a finder pattern whose top left is row and col, with its light separator.
func (m *matrix) finder(row, col int) {
	for dr := -1; dr <= 7; dr++ {
		for dc := -1; dc <= 7; dc++ {
			r, c := row+dr, col+dc
			if r < 0 || c < 0 || r >= m.size || c >= m.size {
				continue
			}
			ring := max(abs(dr-3), abs(dc-3))
			m.set(r, c, ring != 2 && ring != 4)
		}
	}
}

// FormatBits is the 15 bits of format information for level M and mask: the level and mask, their
// BCH error correction, and the XOR mask 101010000010010 (ISO/IEC 18004, 7.9).
func FormatBits(mask int) int {
	const levelM = 0b00
	data := levelM<<3 | mask
	rem := data
	for range 10 {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem&0x3FF) ^ 0x5412
}

// format draws the format information for mask in both its places, and the dark module beside the
// lower left finder.
func (m *matrix) format(mask int) {
	bits := FormatBits(mask)
	bit := func(i int) bool { return bits>>i&1 != 0 }
	// Bit 0 is the least significant.
	for i := range 6 {
		m.set(i, 8, bit(i))
	}
	m.set(7, 8, bit(6))
	m.set(8, 8, bit(7))
	m.set(8, 7, bit(8))
	for i := 9; i < 15; i++ {
		m.set(8, 14-i, bit(i))
	}
	for i := range 8 {
		m.set(8, m.size-1-i, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.set(m.size-15+i, 8, bit(i))
	}
	m.set(m.size-8, 8, true)
}

// place lays the codewords in the modules that aren't function patterns: in pairs of columns from
// the right, up the first pair and down the next, skipping the vertical timing pattern. Modules left
// over are the remainder bits, which are light.
func (m *matrix) place(codewords []byte) {
	i := 0
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		upward := (right+1)&2 == 0
		for vert := range m.size {
			row := vert
			if upward {
				row = m.size - 1 - vert
			}
			for j := range 2 {
				col := right - j
				if m.function[row*m.size+col] || i >= len(codewords)*8 {
					continue
				}
				m.dark[row*m.size+col] = codewords[i/8]>>(7-i%8)&1 != 0
				i++
			}
		}
	}
}

// masked reports whether mask flips the module at row and col (ISO/IEC 18004, table 10).
func masked(mask, row, col int) bool {
	switch mask {
	case 0:
		return (row+col)%2 == 0
	case 1:
		return row%2 == 0
	case 2:
		return col%3 == 0
	case 3:
		return (row+col)%3 == 0
	case 4:
		return (row/2+col/3)%2 == 0
	case 5:
		return row*col%2+row*col%3 == 0
	case 6:
		return (row*col%2+row*col%3)%2 == 0
	default:
		return ((row+col)%2+row*col%3)%2 == 0
	}
}

func (m *matrix) applyMask(mask int) {
	for row := range m.size {
		for col := range m.size {
			if !m.function[row*m.size+col] && masked(mask, row, col) {
				m.dark[row*m.size+col] = !m.dark[row*m.size+col]
			}
		}
	}
}

// penalty scores how hard c is to read, by the four rules that choose its mask (ISO/IEC 18004,
// 7.8.3): runs of one colour, 2×2 blocks of one colour, patterns that look like a finder's, and an
// imbalance of dark and light.
func penalty(c *Code) int {
	n := c.Size
	score := 0
	lines := func(dark func(i, j int) bool) {
		for i := range n {
			run := 1
			for j := 1; j <= n; j++ {
				if j < n && dark(i, j) == dark(i, j-1) {
					run++
					continue
				}
				if run >= 5 {
					score += 3 + run - 5
				}
				run = 1
			}
			// 1:1:3:1:1, with four light modules on one side. Outside the code is light.
			for j := -4; j < n; j++ {
				finder := true
				for k, want := range [7]bool{true, false, true, true, true, false, true} {
					if dark(i, j+k) != want {
						finder = false
						break
					}
				}
				if !finder {
					continue
				}
				before, after := true, true
				for k := 1; k <= 4; k++ {
					before = before && !dark(i, j-k)
					after = after && !dark(i, j+6+k)
				}
				if before {
					score += 40
				}
				if after {
					score += 40
				}
			}
		}
	}
	lines(func(i, j int) bool { return c.Dark(i, j) })
	lines(func(i, j int) bool { return c.Dark(j, i) })
	dark := 0
	for row := range n {
		for col := range n {
			if c.Dark(row, col) {
				dark++
			}
			if row+1 < n && col+1 < n {
				d := c.Dark(row, col)
				if c.Dark(row+1, col) == d && c.Dark(row, col+1) == d && c.Dark(row+1, col+1) == d {
					score += 3
				}
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	return score + 10*k
}
