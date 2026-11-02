package tlv

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
)

// specExamples are the encoding examples in the Matter Core Specification, Appendix A.12.
var specExamples = []struct {
	name, hex string
}{
	{"Boolean false", "08"},
	{"Boolean true", "09"},
	{"Signed Integer, 1-octet, value 42", "00 2a"},
	{"Signed Integer, 1-octet, value -17", "00 ef"},
	{"Unsigned Integer, 1-octet, value 42U", "04 2a"},
	{"Signed Integer, 2-octet, value 42", "01 2a 00"},
	{"Signed Integer, 4-octet, value -170000", "02 f0 67 fd ff"},
	{"Signed Integer, 8-octet, value 40000000000", "03 00 90 2f 50 09 00 00 00"},
	{"UTF-8 String, 1-octet length, \"Hello!\"", "0c 06 48 65 6c 6c 6f 21"},
	{"UTF-8 String, 1-octet length, \"Tschüs\"", "0c 07 54 73 63 68 c3 bc 73"},
	{"Octet String, 1-octet length, octets 00 01 02 03 04", "10 05 00 01 02 03 04"},
	{"Null", "14"},
	{"Single precision floating point 0.0", "0a 00 00 00 00"},
	{"Single precision floating point (1.0 / 3.0)", "0a ab aa aa 3e"},
	{"Single precision floating point 17.9", "0a 33 33 8f 41"},
	{"Single precision floating point infinity", "0a 00 00 80 7f"},
	{"Single precision floating point negative infinity", "0a 00 00 80 ff"},
	{"Double precision floating point 0.0", "0b 00 00 00 00 00 00 00 00"},
	{"Double precision floating point (1.0 / 3.0)", "0b 55 55 55 55 55 55 d5 3f"},
	{"Double precision floating point 17.9", "0b 66 66 66 66 66 e6 31 40"},
	{"Double precision floating point infinity", "0b 00 00 00 00 00 00 f0 7f"},
	{"Double precision floating point negative infinity", "0b 00 00 00 00 00 00 f0 ff"},
	{"Empty Structure, {}", "15 18"},
	{"Empty Array, []", "16 18"},
	{"Empty List, [[]]", "17 18"},
	{"Structure, two context specific tags, Signed Integer, 1 octet values, {0 = 42, 1 = -17}", "15 20 00 2a 20 01 ef 18"},
	{"Array, Signed Integer, 1-octet values, [0, 1, 2, 3, 4]", "16 00 00 00 01 00 02 00 03 00 04 18"},
	{"List, mix of anonymous and context tags, Signed Integer, 1 octet values, [[1, 0 = 42, 2, 3, 0 = -17]]", "17 00 01 20 00 2a 00 02 00 03 20 00 ef 18"},
	{"Array, mix of element types, [42, -170000, {}, 17.9, \"Hello!\"]", "16 00 2a 02 f0 67 fd ff 15 18 0a 33 33 8f 41 0c 06 48 65 6c 6c 6f 21 18"},
	{"Anonymous tag, Unsigned Integer, 1-octet value, 42U", "04 2a"},
	{"Context tag 1, Unsigned Integer, 1-octet value, 1 = 42U", "24 01 2a"},
	{"Common profile tag 1, Unsigned Integer, 1-octet value, Matter::1 = 42U", "44 01 00 2a"},
	{"Common profile tag 100000, Unsigned Integer, 1-octet value, Matter::100000 = 42U", "64 a0 86 01 00 2a"},
	{"Fully qualified tag, 2-octet tag 1, 65521::57069:1 = 42U", "c4 f1 ff ed de 01 00 2a"},
	{"Fully qualified tag, 4-octet tag 0xAA55FEED, 65521::57069:2857762541 = 42U", "e4 f1 ff ed de ed fe 55 aa 2a"},
	{"Structure with a fully qualified tag, 65521::57069:1 = {65521::57069:43605 = 42U}", "d5 f1 ff ed de 01 00 c4 f1 ff ed de 55 aa 2a 18"},
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSpecExamplesRoundTrip(t *testing.T) {
	for _, ex := range specExamples {
		t.Run(ex.name, func(t *testing.T) {
			in := unhex(t, ex.hex)
			var w Writer
			if err := copyTLV(NewReader(in), &w); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := w.Bytes(); !bytes.Equal(got, in) {
				t.Errorf("re-encoded as % x, want % x", got, in)
			}
		})
	}
}

// TestSpecExamplesEncode builds some of the examples from values, with default widths.
func TestSpecExamplesEncode(t *testing.T) {
	cases := []struct {
		hex   string
		write func(w *Writer)
	}{
		{"00 ef", func(w *Writer) { w.PutInt(Anonymous, -17) }},
		{"02 f0 67 fd ff", func(w *Writer) { w.PutInt(Anonymous, -170000) }},
		{"03 00 90 2f 50 09 00 00 00", func(w *Writer) { w.PutInt(Anonymous, 40000000000) }},
		{"0c 07 54 73 63 68 c3 bc 73", func(w *Writer) { w.PutUTF8(Anonymous, "Tschüs") }},
		{"0a 33 33 8f 41", func(w *Writer) { w.PutFloat32(Anonymous, 17.9) }},
		{"0b 66 66 66 66 66 e6 31 40", func(w *Writer) { w.PutFloat64(Anonymous, 17.9) }},
		{"15 20 00 2a 20 01 ef 18", func(w *Writer) {
			w.StartStructure(Anonymous)
			w.PutInt(Context(0), 42)
			w.PutInt(Context(1), -17)
			w.End()
		}},
		{"17 00 01 20 00 2a 00 02 00 03 20 00 ef 18", func(w *Writer) {
			w.StartList(Anonymous)
			w.PutInt(Anonymous, 1)
			w.PutInt(Context(0), 42)
			w.PutInt(Anonymous, 2)
			w.PutInt(Anonymous, 3)
			w.PutInt(Context(0), -17)
			w.End()
		}},
		{"64 a0 86 01 00 2a", func(w *Writer) { w.PutUint(CommonProfile(100000), 42) }},
		{"e4 f1 ff ed de ed fe 55 aa 2a", func(w *Writer) { w.PutUint(FullyQualified(0xFFF1, 0xDEED, 0xAA55FEED), 42) }},
		{"d5 f1 ff ed de 01 00 c4 f1 ff ed de 55 aa 2a 18", func(w *Writer) {
			w.StartStructure(FullyQualified(0xFFF1, 0xDEED, 1))
			w.PutUint(FullyQualified(0xFFF1, 0xDEED, 0xAA55), 42)
			w.End()
		}},
	}
	for _, c := range cases {
		var w Writer
		c.write(&w)
		if want := unhex(t, c.hex); !bytes.Equal(w.Bytes(), want) {
			t.Errorf("encoded % x, want % x", w.Bytes(), want)
		}
	}
}

func TestReadValues(t *testing.T) {
	r := NewReader(unhex(t, "16 00 2a 02 f0 67 fd ff 15 18 0a 33 33 8f 41 0c 06 48 65 6c 6c 6f 21 18"))
	must(t, r.Next())
	must(t, r.Enter())

	must(t, r.Next())
	if v, err := r.Int(); err != nil || v != 42 {
		t.Errorf("Int() = %d, %v; want 42", v, err)
	}
	must(t, r.Next())
	if v, err := r.Int(); err != nil || v != -170000 {
		t.Errorf("Int() = %d, %v; want -170000", v, err)
	}
	must(t, r.Next())
	if r.Type() != Structure {
		t.Errorf("Type() = %s, want structure", r.Type())
	}
	must(t, r.Next()) // skips the empty structure without entering it
	if v, err := r.Float(); err != nil || float32(v) != 17.9 {
		t.Errorf("Float() = %v, %v; want 17.9", v, err)
	}
	must(t, r.Next())
	if v, err := r.UTF8(); err != nil || v != "Hello!" {
		t.Errorf("UTF8() = %q, %v; want Hello!", v, err)
	}
	if err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() at the end of the array = %v, want io.EOF", err)
	}
	must(t, r.Exit())
	if err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next() at the end of the input = %v, want io.EOF", err)
	}
}

func TestExitSkipsTheRest(t *testing.T) {
	// {0 = [1, {}], 1 = "x"}, then 7U at the top level.
	r := NewReader(unhex(t, "15 36 00 00 01 15 18 18 2c 01 01 78 18 04 07"))
	must(t, r.Next())
	must(t, r.Enter())
	must(t, r.Next())
	must(t, r.Enter()) // into the array, then leave it at once
	must(t, r.Exit())
	must(t, r.Next())
	if r.Tag() != Context(1) || r.Type() != UTF8 {
		t.Fatalf("after the array: %s %s, want 1 utf8", r.Tag(), r.Type())
	}
	must(t, r.Exit())
	must(t, r.Next())
	if v, err := r.Uint(); err != nil || v != 7 {
		t.Errorf("Uint() = %d, %v; want 7", v, err)
	}
}

func TestTypeMismatch(t *testing.T) {
	r := NewReader(unhex(t, "04 2a"))
	must(t, r.Next())
	_, err := r.Int()
	var te *ErrType
	if !errors.As(err, &te) || te.Want != Int || te.Got != Uint || !errors.Is(err, ErrInvalid) {
		t.Errorf("Int() on a uint: %v", err)
	}
	if v, err := r.Uint(); err != nil || v != 42 {
		t.Errorf("a type mismatch must not stick: Uint() = %d, %v", v, err)
	}
}

func TestMalformed(t *testing.T) {
	deep := strings.Repeat("17", MaxDepth+1) + strings.Repeat("18", MaxDepth+1)
	cases := map[string]string{
		"truncated int":                  "01 2a",
		"truncated float":                "0b 00 00",
		"truncated string":               "0c 06 48 65",
		"truncated length":               "0d 06",
		"overlong 8-byte length":         "0f ff ff ff ff ff ff ff ff 00",
		"length past the end":            "10 05 00 01",
		"truncated tag":                  "24",
		"truncated fully qualified tag":  "c4 f1 ff ed",
		"reserved type":                  "19",
		"reserved type 0x1f":             "1f",
		"end of container at top level":  "18",
		"end of container with a tag":    "15 38 00",
		"unclosed structure":             "15 24 00 01",
		"unclosed nested":                "15 35 01 18",
		"anonymous element in structure": "15 00 2a 18",
		"tagged element in array":        "16 20 00 2a 18",
		"too deep":                       deep,
		"invalid UTF-8":                  "0c 01 ff",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			err := walk(NewReader(unhex(t, h)))
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("walk() = %v, want an ErrInvalid", err)
			}
		})
	}
}

func TestSkipTooDeep(t *testing.T) {
	r := NewReader(unhex(t, "16"+strings.Repeat("17", MaxDepth)+strings.Repeat("18", MaxDepth+1)))
	must(t, r.Next())
	if err := r.Next(); !errors.Is(err, ErrTooDeep) {
		t.Errorf("skipping a container nested too deep: %v, want ErrTooDeep", err)
	}
}

func TestErrorsStick(t *testing.T) {
	r := NewReader(unhex(t, "19 04 2a"))
	first := r.Next()
	if !errors.Is(first, ErrInvalid) {
		t.Fatalf("Next() = %v, want ErrInvalid", first)
	}
	if err := r.Next(); err != first {
		t.Errorf("second Next() = %v, want the first error again", err)
	}
}

func TestWriterPanics(t *testing.T) {
	cases := map[string]func(w *Writer){
		"anonymous in structure": func(w *Writer) { w.StartStructure(Anonymous); w.PutBool(Anonymous, true) },
		"tagged in array":        func(w *Writer) { w.StartArray(Anonymous); w.PutBool(Context(1), true) },
		"end without start":      func(w *Writer) { w.End() },
		"open container":         func(w *Writer) { w.StartList(Anonymous); w.Bytes() },
		"int too wide":           func(w *Writer) { w.PutIntSized(Anonymous, 128, 1) },
		"uint too wide":          func(w *Writer) { w.PutUintSized(Anonymous, 256, 1) },
		"bad width":              func(w *Writer) { w.PutUintSized(Anonymous, 1, 3) },
		"context tag over 255":   func(w *Writer) { w.PutNull(Tag{Kind: ContextTag, Number: 256}) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("didn't panic")
				}
			}()
			f(&Writer{})
		})
	}
}

func TestIntWidths(t *testing.T) {
	for _, v := range []int64{0, -1, math.MinInt8, math.MaxInt8, math.MinInt8 - 1, math.MaxInt8 + 1,
		math.MinInt16, math.MaxInt16, math.MinInt32, math.MaxInt32, math.MinInt32 - 1, math.MinInt64, math.MaxInt64} {
		var w Writer
		w.PutInt(Anonymous, v)
		r := NewReader(w.Bytes())
		must(t, r.Next())
		if got, err := r.Int(); err != nil || got != v {
			t.Errorf("Int() = %d, %v; want %d", got, err, v)
		}
	}
	for _, v := range []uint64{0, math.MaxUint8, math.MaxUint8 + 1, math.MaxUint16 + 1, math.MaxUint32, math.MaxUint32 + 1, math.MaxUint64} {
		var w Writer
		w.PutUint(Anonymous, v)
		r := NewReader(w.Bytes())
		must(t, r.Next())
		if got, err := r.Uint(); err != nil || got != v {
			t.Errorf("Uint() = %d, %v; want %d", got, err, v)
		}
	}
}

// FuzzReader checks that no input makes the Reader panic, and that whatever it accepts encodes
// back to the same elements.
func FuzzReader(f *testing.F) {
	for _, ex := range specExamples {
		f.Add(unhex(f, ex.hex))
	}
	f.Add(unhex(f, "0f ff ff ff ff ff ff ff ff 00"))
	f.Add(unhex(f, "15 36 00 00 01 15 18 18 2c 01 01 78 18 04 07"))
	f.Fuzz(func(t *testing.T, in []byte) {
		want, err := dump(NewReader(in))
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error %v doesn't wrap ErrInvalid", err)
			}
			return
		}
		var w Writer
		if err := copyTLV(NewReader(in), &w); err != nil {
			t.Fatalf("dump accepted the input but copy failed: %v", err)
		}
		got, err := dump(NewReader(w.Bytes()))
		if err != nil || got != want {
			t.Fatalf("re-encoded % x reads as %q, %v; want %q", w.Bytes(), got, err, want)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// walk reads every element and value in r.
func walk(r *Reader) error {
	_, err := dump(r)
	return err
}

// dump renders every element in r, one per line.
func dump(r *Reader) (string, error) {
	var b strings.Builder
	err := visit(r, func(r *Reader) error {
		var v any
		var err error
		switch r.Type() {
		case Int:
			v, err = r.Int()
		case Uint:
			v, err = r.Uint()
		case Bool:
			v, err = r.Bool()
		case Float:
			if r.Size() == 4 {
				var f float32
				f, err = r.Float32()
				v = math.Float32bits(f)
			} else {
				var f float64
				f, err = r.Float()
				v = math.Float64bits(f)
			}
		case UTF8:
			v, err = r.UTF8()
		case Bytes:
			v, err = r.Bytes()
		}
		fmt.Fprintf(&b, "%*s%s %s %v\n", r.Depth(), "", r.Tag(), r.Type(), v)
		return err
	}, nil)
	return b.String(), err
}

// copyTLV writes every element in r to w, keeping the widths of integers.
func copyTLV(r *Reader, w *Writer) error {
	return visit(r, func(r *Reader) error {
		tag := r.Tag()
		switch r.Type() {
		case Int:
			v, err := r.Int()
			if err != nil {
				return err
			}
			w.PutIntSized(tag, v, r.Size())
		case Uint:
			v, err := r.Uint()
			if err != nil {
				return err
			}
			w.PutUintSized(tag, v, r.Size())
		case Bool:
			v, err := r.Bool()
			if err != nil {
				return err
			}
			w.PutBool(tag, v)
		case Float:
			if r.Size() == 4 {
				v, err := r.Float32()
				if err != nil {
					return err
				}
				w.PutFloat32(tag, v)
			} else {
				v, err := r.Float()
				if err != nil {
					return err
				}
				w.PutFloat64(tag, v)
			}
		case UTF8:
			v, err := r.UTF8()
			if err != nil {
				return err
			}
			w.PutUTF8(tag, v)
		case Bytes:
			v, err := r.Bytes()
			if err != nil {
				return err
			}
			w.PutBytes(tag, v)
		case Null:
			w.PutNull(tag)
		case Structure:
			w.StartStructure(tag)
		case Array:
			w.StartArray(tag)
		case List:
			w.StartList(tag)
		}
		return nil
	}, w.End)
}

// visit calls each for every element in r, depth first, and end after each container's elements.
func visit(r *Reader, each func(*Reader) error, end func()) error {
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := each(r); err != nil {
			return err
		}
		if !r.Type().IsContainer() {
			continue
		}
		if err := r.Enter(); err != nil {
			return err
		}
		if err := visit(r, each, end); err != nil {
			return err
		}
		if err := r.Exit(); err != nil {
			return err
		}
		if end != nil {
			end()
		}
	}
}
