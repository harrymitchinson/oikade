package qr

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Level M's format information for each mask, as ISO/IEC 18004's table C.1 lists it.
func TestFormatBits(t *testing.T) {
	want := []string{
		"101010000010010", "101000100100101", "101111001111100", "101101101001011",
		"100010111111001", "100000011001110", "100111110010111", "100101010100000",
	}
	for mask, w := range want {
		if got := fmt.Sprintf("%015b", FormatBits(mask)); got != w {
			t.Errorf("mask %d: format bits %s, want %s", mask, got, w)
		}
	}
}

// HELLO WORLD at 1-M, the worked example of Thonky's QR code tutorial.
func TestHelloWorldCodewords(t *testing.T) {
	data := dataCodewords("HELLO WORLD", 1)
	wantData := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	if !bytes.Equal(data, wantData) {
		t.Fatalf("data codewords %v, want %v", data, wantData)
	}
	ec := remainder(data, generator(10))
	wantEC := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if !bytes.Equal(ec, wantEC) {
		t.Fatalf("error correction codewords %v, want %v", ec, wantEC)
	}
}

// Each block's codewords, data and error correction, are a Reed-Solomon codeword: zero at each of
// the generator's roots.
func TestErrorCorrectionHasNoSyndrome(t *testing.T) {
	for v := 1; v < len(versions); v++ {
		ver := versions[v]
		data := make([]byte, ver.data, ver.data+ver.ec)
		for i := range data {
			data[i] = byte(i*37 + v)
		}
		block := append(data, remainder(data, generator(ver.ec))...)
		for root := range ver.ec {
			var s byte
			for _, c := range block {
				s = gfMul(s, gfExp[root]) ^ c
			}
			if s != 0 {
				t.Errorf("version %d: syndrome %d is %d", v, root, s)
			}
		}
	}
}

func TestEncodeRoundTrips(t *testing.T) {
	for _, text := range []string{
		"MT:Y.K9042C00KA0648G00",
		"MT:-24J0AFN00KA0648G00",
		"A",
		"HELLO WORLD",
		strings.Repeat("0123456789ABCDEF", 6),
		strings.Repeat("$%*+-./: ", 18)[:154],
	} {
		c, err := Encode(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		got, err := decode(c)
		if err != nil {
			t.Fatalf("%q at version %d mask %d: %v", text, c.Version, c.Mask, err)
		}
		if got != text {
			t.Fatalf("decoded %q, want %q", got, text)
		}
	}
}

func TestMatterPayloadIsVersion2(t *testing.T) {
	c, err := Encode("MT:Y.K9042C00KA0648G00")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 2 || c.Size != 25 {
		t.Fatalf("version %d, size %d; want 2, 25", c.Version, c.Size)
	}
}

// Every mask makes a code that reads, not only the one Encode chooses.
func TestEveryMaskRoundTrips(t *testing.T) {
	const text = "MT:Y.K9042C00KA0648G00"
	codewords := interleave(dataCodewords(text, 2), 2)
	for mask := range 8 {
		m := newMatrix(25, 2)
		m.place(codewords)
		m.applyMask(mask)
		m.format(mask)
		got, err := decode(&Code{Size: 25, Version: 2, Mask: mask, dark: m.dark})
		if err != nil || got != text {
			t.Errorf("mask %d: decoded %q, %v", mask, got, err)
		}
	}
}

func TestEncodeRefuses(t *testing.T) {
	for _, text := range []string{"mt:lower", "MT:é", strings.Repeat("A", 155)} {
		if _, err := Encode(text); err == nil {
			t.Errorf("%q: encoded", text)
		}
	}
}

func TestTerminal(t *testing.T) {
	c, err := Encode("MT:Y.K9042C00KA0648G00")
	if err != nil {
		t.Fatal(err)
	}
	lines := c.Terminal()
	if len(lines) != (25+8+1)/2 {
		t.Fatalf("%d lines, want 17", len(lines))
	}
	for i, line := range lines {
		body, coloured := strings.CutPrefix(line, colours)
		body, ended := strings.CutSuffix(body, reset)
		if !coloured || !ended {
			t.Fatalf("line %d isn't coloured: %q", i, line)
		}
		row := []rune(body)
		if len(row) != 25+8 {
			t.Fatalf("line %d is %d wide, want 33", i, len(row))
		}
		for j, r := range row {
			top, bottom := c.Dark(2*i-4, j-4), c.Dark(2*i-3, j-4)
			want := map[[2]bool]rune{{false, false}: ' ', {true, false}: '▀', {false, true}: '▄', {true, true}: '█'}[[2]bool{top, bottom}]
			if r != want {
				t.Fatalf("line %d column %d is %q, want %q", i, j, r, want)
			}
		}
	}
	// The quiet zone: the first two lines are blank.
	for _, line := range lines[:2] {
		if strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, colours), reset)) != "" {
			t.Fatalf("quiet zone has a dark module: %q", line)
		}
	}
}

// decode reads c back as a reader would, written apart from the encoder: it checks the finders and
// timing patterns, reads the format information from where it lies beside the top left finder,
// unmasks, reads the codewords in their zigzag, de-interleaves them, checks each block's error
// correction, and parses the alphanumeric segment.
func decode(c *Code) (string, error) {
	v := (c.Size - 17) / 4
	if v < 1 || v >= len(versions) || 17+4*v != c.Size {
		return "", fmt.Errorf("size %d", c.Size)
	}
	if err := checkPatterns(c); err != nil {
		return "", err
	}
	mask, err := readFormat(c)
	if err != nil {
		return "", err
	}
	codewords, err := readCodewords(c, v, mask)
	if err != nil {
		return "", err
	}
	data, err := correct(codewords, versions[v])
	if err != nil {
		return "", err
	}
	return parse(data)
}

func checkPatterns(c *Code) error {
	n := c.Size
	for _, at := range [][2]int{{0, 0}, {0, n - 7}, {n - 7, 0}} {
		for r := range 7 {
			for k := range 7 {
				edge := r == 0 || r == 6 || k == 0 || k == 6
				centre := r >= 2 && r <= 4 && k >= 2 && k <= 4
				if c.Dark(at[0]+r, at[1]+k) != (edge || centre) {
					return fmt.Errorf("finder at %v is wrong at %d,%d", at, r, k)
				}
			}
		}
	}
	for i := 8; i < n-8; i++ {
		if c.Dark(6, i) != (i%2 == 0) || c.Dark(i, 6) != (i%2 == 0) {
			return fmt.Errorf("timing pattern is wrong at %d", i)
		}
	}
	if !c.Dark(n-8, 8) {
		return fmt.Errorf("no dark module")
	}
	return nil
}

// readFormat reads the format information, most significant bit first: along row 8 beside the top
// left finder, then up column 8, skipping the timing pattern's column and row. It returns the mask.
func readFormat(c *Code) (int, error) {
	var bits int
	for _, at := range [][2]int{
		{8, 0}, {8, 1}, {8, 2}, {8, 3}, {8, 4}, {8, 5}, {8, 7}, {8, 8},
		{7, 8}, {5, 8}, {4, 8}, {3, 8}, {2, 8}, {1, 8}, {0, 8},
	} {
		bits <<= 1
		if c.Dark(at[0], at[1]) {
			bits |= 1
		}
	}
	format := bits ^ 0x5412
	if format>>13 != 0b00 {
		return 0, fmt.Errorf("format information %015b isn't level M", format)
	}
	mask := format >> 10 & 7
	if FormatBits(mask) != bits {
		return 0, fmt.Errorf("format information %015b has a bad BCH code", bits)
	}
	return mask, nil
}

// readCodewords reads the modules that carry data, unmasked, up and down each pair of columns from
// the right.
func readCodewords(c *Code, v, mask int) ([]byte, error) {
	n := c.Size
	// Not the finders with their separators and format information, the timing patterns, or the
	// alignment pattern.
	isData := func(row, col int) bool {
		switch {
		case row == 6 || col == 6:
			return false
		case row <= 8 && col <= 8, row <= 8 && col >= n-8, row >= n-8 && col <= 8:
			return false
		case v >= 2 && row >= n-9 && row <= n-5 && col >= n-9 && col <= n-5:
			return false
		}
		return true
	}
	var bits []bool
	up := true
	for col := n - 1; col > 0; col -= 2 {
		if col == 6 {
			col--
		}
		for k := range n {
			row := k
			if up {
				row = n - 1 - k
			}
			for _, cc := range []int{col, col - 1} {
				if isData(row, cc) {
					bits = append(bits, c.Dark(row, cc) != masked(mask, row, cc))
				}
			}
		}
		up = !up
	}
	ver := versions[v]
	total := ver.blocks * (ver.data + ver.ec)
	if len(bits) != total*8+ver.remainder {
		return nil, fmt.Errorf("%d data modules, want %d", len(bits), total*8+ver.remainder)
	}
	codewords := make([]byte, total)
	for i := range codewords {
		for b := range 8 {
			if bits[i*8+b] {
				codewords[i] |= 0x80 >> b
			}
		}
	}
	return codewords, nil
}

// correct de-interleaves the blocks, checks that each one's syndromes are zero, and returns their
// data codewords.
func correct(codewords []byte, ver version) ([]byte, error) {
	var data []byte
	for i := range ver.blocks {
		block := make([]byte, 0, ver.data+ver.ec)
		for j := range ver.data {
			block = append(block, codewords[j*ver.blocks+i])
		}
		for j := range ver.ec {
			block = append(block, codewords[ver.blocks*ver.data+j*ver.blocks+i])
		}
		for root := range ver.ec {
			var s byte
			for _, x := range block {
				s = gfMul(s, gfExp[root]) ^ x
			}
			if s != 0 {
				return nil, fmt.Errorf("block %d doesn't check", i)
			}
		}
		data = append(data, block[:ver.data]...)
	}
	return data, nil
}

func parse(data []byte) (string, error) {
	pos := 0
	read := func(width int) int {
		x := 0
		for range width {
			x = x<<1 | int(data[pos/8]>>(7-pos%8)&1)
			pos++
		}
		return x
	}
	if m := read(4); m != 0b0010 {
		return "", fmt.Errorf("mode %04b", m)
	}
	count := read(9)
	var out strings.Builder
	for ; count >= 2; count -= 2 {
		x := read(11)
		out.WriteByte(alphanumeric[x/45])
		out.WriteByte(alphanumeric[x%45])
	}
	if count == 1 {
		out.WriteByte(alphanumeric[read(6)])
	}
	return out.String(), nil
}
