package tlv

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Writer encodes TLV elements into a growing buffer. Integers, string lengths and tags take the
// fewest bytes that hold them, unless a Sized method asks for a width.
//
// Misuse, such as an anonymous element in a structure or End with no open container, is a bug in
// the caller, and panics.
type Writer struct {
	buf   []byte
	stack []Type
}

// Bytes returns the encoding so far. It panics if a container is still open.
func (w *Writer) Bytes() []byte {
	if len(w.stack) > 0 {
		panic(fmt.Sprintf("tlv: %s still open", w.stack[len(w.stack)-1]))
	}
	return w.buf
}

// PutInt writes a signed integer in the fewest bytes that hold it.
func (w *Writer) PutInt(tag Tag, v int64) {
	switch {
	case v >= math.MinInt8 && v <= math.MaxInt8:
		w.PutIntSized(tag, v, 1)
	case v >= math.MinInt16 && v <= math.MaxInt16:
		w.PutIntSized(tag, v, 2)
	case v >= math.MinInt32 && v <= math.MaxInt32:
		w.PutIntSized(tag, v, 4)
	default:
		w.PutIntSized(tag, v, 8)
	}
}

// PutIntSized writes a signed integer in size bytes: 1, 2, 4 or 8. It panics if v doesn't fit.
func (w *Writer) PutIntSized(tag Tag, v int64, size int) {
	code := sizeCode(size)
	if bits := uint(size * 8); bits < 64 && (v < -1<<(bits-1) || v > 1<<(bits-1)-1) {
		panic(fmt.Sprintf("tlv: %d doesn't fit in %d bytes", v, size))
	}
	w.head(tag, typeInt8+code)
	w.buf = appendLE(w.buf, uint64(v), size)
}

// PutUint writes an unsigned integer in the fewest bytes that hold it.
func (w *Writer) PutUint(tag Tag, v uint64) {
	w.PutUintSized(tag, v, uintSize(v))
}

// PutUintSized writes an unsigned integer in size bytes: 1, 2, 4 or 8. It panics if v doesn't
// fit.
func (w *Writer) PutUintSized(tag Tag, v uint64, size int) {
	code := sizeCode(size)
	if uintSize(v) > size {
		panic(fmt.Sprintf("tlv: %d doesn't fit in %d bytes", v, size))
	}
	w.head(tag, typeUint8+code)
	w.buf = appendLE(w.buf, v, size)
}

// PutBool writes a boolean.
func (w *Writer) PutBool(tag Tag, v bool) {
	if v {
		w.head(tag, typeTrue)
	} else {
		w.head(tag, typeFalse)
	}
}

// PutFloat32 writes a single precision float.
func (w *Writer) PutFloat32(tag Tag, v float32) {
	w.head(tag, typeFloat32)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(v))
}

// PutFloat64 writes a double precision float.
func (w *Writer) PutFloat64(tag Tag, v float64) {
	w.head(tag, typeFloat64)
	w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(v))
}

// PutUTF8 writes a UTF-8 string. It doesn't check that s is valid UTF-8.
func (w *Writer) PutUTF8(tag Tag, s string) {
	w.putLength(tag, typeUTF8Len1, len(s))
	w.buf = append(w.buf, s...)
}

// PutBytes writes an octet string.
func (w *Writer) PutBytes(tag Tag, b []byte) {
	w.putLength(tag, typeBytesLen1, len(b))
	w.buf = append(w.buf, b...)
}

// PutNull writes a null.
func (w *Writer) PutNull(tag Tag) { w.head(tag, typeNull) }

// StartStructure opens a structure, whose elements must be tagged. End closes it.
func (w *Writer) StartStructure(tag Tag) { w.start(tag, Structure, typeStructure) }

// StartArray opens an array, whose elements must be anonymous. End closes it.
func (w *Writer) StartArray(tag Tag) { w.start(tag, Array, typeArray) }

// StartList opens a list, whose elements may be tagged or not. End closes it.
func (w *Writer) StartList(tag Tag) { w.start(tag, List, typeList) }

// PutRaw appends one element that is already encoded, such as one built by another Writer. Its
// tag must suit the open container: anonymous in an array, tagged in a structure. It panics if
// it doesn't.
func (w *Writer) PutRaw(element []byte) {
	if len(element) == 0 {
		panic("tlv: PutRaw of nothing")
	}
	tag, _, err := parseTag(element, 1, element[0]&tagMask)
	if err != nil {
		panic(fmt.Sprintf("tlv: PutRaw of a malformed element: %v", err))
	}
	if len(w.stack) > 0 {
		switch parent := w.stack[len(w.stack)-1]; {
		case parent == Structure && tag.Kind == AnonymousTag:
			panic("tlv: anonymous element in a structure")
		case parent == Array && tag.Kind != AnonymousTag:
			panic(fmt.Sprintf("tlv: tagged element %s in an array", tag))
		}
	}
	w.buf = append(w.buf, element...)
}

// Len returns how many bytes the Writer holds.
func (w *Writer) Len() int { return len(w.buf) }

// End closes the container opened last.
func (w *Writer) End() {
	if len(w.stack) == 0 {
		panic("tlv: End with no open container")
	}
	w.stack = w.stack[:len(w.stack)-1]
	w.buf = append(w.buf, typeEnd)
}

func (w *Writer) start(tag Tag, t Type, code byte) {
	if len(w.stack)+1 > MaxDepth {
		panic(fmt.Sprintf("tlv: containers nested more than %d deep", MaxDepth))
	}
	w.head(tag, code)
	w.stack = append(w.stack, t)
}

func (w *Writer) putLength(tag Tag, base byte, n int) {
	size := uintSize(uint64(n))
	w.head(tag, base+sizeCode(size))
	w.buf = appendLE(w.buf, uint64(n), size)
}

// head writes an element's control byte and tag.
func (w *Writer) head(tag Tag, code byte) {
	if len(w.stack) > 0 {
		switch parent := w.stack[len(w.stack)-1]; {
		case parent == Structure && tag.Kind == AnonymousTag:
			panic("tlv: anonymous element in a structure")
		case parent == Array && tag.Kind != AnonymousTag:
			panic(fmt.Sprintf("tlv: tagged element %s in an array", tag))
		}
	}
	switch tag.Kind {
	case AnonymousTag:
		w.buf = append(w.buf, tagAnonymous|code)
	case ContextTag:
		if tag.Number > math.MaxUint8 {
			panic(fmt.Sprintf("tlv: context tag %d is over 255", tag.Number))
		}
		w.buf = append(w.buf, tagContext|code, byte(tag.Number))
	case CommonProfileTag:
		w.profileTag(tag.Number, tagCommon2|code, tagCommon4|code)
	case ImplicitProfileTag:
		w.profileTag(tag.Number, tagImplicit2|code, tagImplicit4|code)
	case FullyQualifiedTag:
		ctl := byte(tagFullyQualify8)
		if tag.Number <= math.MaxUint16 {
			ctl = tagFullyQualify6
		}
		w.buf = append(w.buf, ctl|code)
		w.buf = binary.LittleEndian.AppendUint16(w.buf, tag.Vendor)
		w.buf = binary.LittleEndian.AppendUint16(w.buf, tag.Profile)
		w.profileNumber(tag.Number)
	default:
		panic(fmt.Sprintf("tlv: unknown tag kind %d", tag.Kind))
	}
}

func (w *Writer) profileTag(n uint32, ctl2, ctl4 byte) {
	if n <= math.MaxUint16 {
		w.buf = append(w.buf, ctl2)
	} else {
		w.buf = append(w.buf, ctl4)
	}
	w.profileNumber(n)
}

func (w *Writer) profileNumber(n uint32) {
	if n <= math.MaxUint16 {
		w.buf = binary.LittleEndian.AppendUint16(w.buf, uint16(n))
	} else {
		w.buf = binary.LittleEndian.AppendUint32(w.buf, n)
	}
}

// appendLE appends the low size bytes of v, little endian.
func appendLE(b []byte, v uint64, size int) []byte {
	for i := range size {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

func uintSize(v uint64) int {
	switch {
	case v <= math.MaxUint8:
		return 1
	case v <= math.MaxUint16:
		return 2
	case v <= math.MaxUint32:
		return 4
	}
	return 8
}

// sizeCode turns a width of 1, 2, 4 or 8 bytes into the 0 to 3 that type codes add.
func sizeCode(size int) byte {
	switch size {
	case 1:
		return 0
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	}
	panic(fmt.Sprintf("tlv: width %d is not 1, 2, 4 or 8", size))
}
