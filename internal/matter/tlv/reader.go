package tlv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"unicode/utf8"
)

// Reader walks TLV elements one at a time without copying or allocating per element.
//
// Next moves to the next element in the current container, or returns io.EOF at its end. Enter
// steps into a container element, and Exit leaves the container, skipping whatever is left in it.
// Next skips over a container that wasn't entered. The first decoding error sticks: every later
// call returns it.
//
//	r := tlv.NewReader(b)
//	for r.Next() == nil { ... }
type Reader struct {
	buf   []byte
	off   int
	stack []Type // the containers entered, innermost last
	err   error

	have bool // positioned on an element
	end  bool // at the end of the current container, or of the input at the top level
	typ  Type
	tag  Tag
	size int    // the value's width for ints and floats, the length field's for strings
	val  []byte // the value's bytes: an int or float, or a string's contents
}

// NewReader returns a Reader over b. The byte and UTF-8 strings it returns alias b.
func NewReader(b []byte) *Reader { return &Reader{buf: b} }

// Next moves to the next element in the current container. It returns io.EOF at the end of the
// container, or at the end of the input at the top level, and an error wrapping ErrInvalid if the
// input is malformed.
func (r *Reader) Next() error {
	if r.err != nil {
		return r.err
	}
	if r.end {
		return io.EOF
	}
	if r.have && r.typ.IsContainer() {
		if err := r.skipContainer(); err != nil {
			return r.fail(err)
		}
	}
	r.have = false
	if r.off >= len(r.buf) {
		if len(r.stack) > 0 {
			return r.fail(fmt.Errorf("%w: %s not closed", ErrTruncate, r.stack[len(r.stack)-1]))
		}
		r.end = true
		return io.EOF
	}
	if r.buf[r.off]&typeMask == typeEnd {
		if err := r.checkEnd(r.buf[r.off], len(r.stack)); err != nil {
			return r.fail(err)
		}
		r.end = true // Exit consumes it
		return io.EOF
	}
	e, next, err := parse(r.buf, r.off, r.parent())
	if err != nil {
		return r.fail(err)
	}
	r.off = next
	r.have = true
	r.typ, r.tag, r.size, r.val = e.typ, e.tag, e.size, e.val
	return nil
}

// Enter steps into the current element, which must be a container. Next then walks its elements.
func (r *Reader) Enter() error {
	if r.err != nil {
		return r.err
	}
	if !r.have || !r.typ.IsContainer() {
		return r.fail(fmt.Errorf("%w: no container to enter", ErrInvalid))
	}
	if len(r.stack)+1 > MaxDepth {
		return r.fail(ErrTooDeep)
	}
	r.stack = append(r.stack, r.typ)
	r.have, r.end = false, false
	return nil
}

// Exit leaves the container last entered, skipping any elements left in it. Next then moves to
// the element after the container.
func (r *Reader) Exit() error {
	if r.err != nil {
		return r.err
	}
	if len(r.stack) == 0 {
		return r.fail(fmt.Errorf("%w: no container to exit", ErrInvalid))
	}
	for !r.end {
		if err := r.Next(); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	r.off++ // the end-of-container byte, checked by Next
	r.stack = r.stack[:len(r.stack)-1]
	r.have, r.end = false, false
	return nil
}

// Depth is how many containers the Reader has entered.
func (r *Reader) Depth() int { return len(r.stack) }

// Type returns the current element's type, or 0 if the Reader isn't on an element.
func (r *Reader) Type() Type {
	if !r.have {
		return 0
	}
	return r.typ
}

// Tag returns the current element's tag.
func (r *Reader) Tag() Tag { return r.tag }

// Size returns the width in bytes of the current element's value for an integer or a float, and
// of its length field for a string. It is 0 for other types.
func (r *Reader) Size() int { return r.size }

// IsNull reports whether the current element is null.
func (r *Reader) IsNull() bool { return r.Type() == Null }

// Int returns the current element's value, which must be a signed integer.
func (r *Reader) Int() (int64, error) {
	if err := r.want(Int); err != nil {
		return 0, err
	}
	switch r.size {
	case 1:
		return int64(int8(r.val[0])), nil
	case 2:
		return int64(int16(binary.LittleEndian.Uint16(r.val))), nil
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(r.val))), nil
	default:
		return int64(binary.LittleEndian.Uint64(r.val)), nil
	}
}

// Uint returns the current element's value, which must be an unsigned integer.
func (r *Reader) Uint() (uint64, error) {
	if err := r.want(Uint); err != nil {
		return 0, err
	}
	return uintLE(r.val), nil
}

// Bool returns the current element's value, which must be a boolean.
func (r *Reader) Bool() (bool, error) {
	if err := r.want(Bool); err != nil {
		return false, err
	}
	return r.val[0] == 1, nil
}

// Float returns the current element's value, which must be a single or double precision float.
func (r *Reader) Float() (float64, error) {
	if err := r.want(Float); err != nil {
		return 0, err
	}
	if r.size == 4 {
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(r.val))), nil
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(r.val)), nil
}

// Float32 returns the current element's value, which must be a single precision float. Unlike
// Float, it keeps a NaN's bits exactly.
func (r *Reader) Float32() (float32, error) {
	if err := r.want(Float); err != nil {
		return 0, err
	}
	if r.size != 4 {
		return 0, fmt.Errorf("%w: a double where a single precision float was expected", ErrInvalid)
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(r.val)), nil
}

// UTF8 returns the current element's value, which must be a valid UTF-8 string.
func (r *Reader) UTF8() (string, error) {
	if err := r.want(UTF8); err != nil {
		return "", err
	}
	if !utf8.Valid(r.val) {
		return "", fmt.Errorf("%w: UTF-8 string %s is not valid UTF-8", ErrInvalid, r.tag)
	}
	return string(r.val), nil
}

// Bytes returns the current element's value, which must be an octet string. The slice aliases
// the Reader's input: copy it to keep it past the input's lifetime.
func (r *Reader) Bytes() ([]byte, error) {
	if err := r.want(Bytes); err != nil {
		return nil, err
	}
	return r.val, nil
}

func (r *Reader) want(t Type) error {
	if r.err != nil {
		return r.err
	}
	if !r.have {
		return fmt.Errorf("%w: not on an element", ErrInvalid)
	}
	if r.typ != t {
		return &ErrType{Want: t, Got: r.typ}
	}
	return nil
}

func (r *Reader) fail(err error) error {
	r.err = err
	r.have = false
	return err
}

func (r *Reader) parent() Type {
	if len(r.stack) == 0 {
		return 0
	}
	return r.stack[len(r.stack)-1]
}

// checkEnd validates an end-of-container byte at the given depth.
func (r *Reader) checkEnd(ctl byte, depth int) error {
	if ctl != typeEnd {
		return fmt.Errorf("%w: end of container with a tag", ErrInvalid)
	}
	if depth == 0 {
		return fmt.Errorf("%w: end of container outside a container", ErrInvalid)
	}
	return nil
}

// skipContainer moves past the current container element, which hasn't been entered, checking
// the elements it skips as strictly as Next would.
func (r *Reader) skipContainer() error {
	stack := []Type{r.typ}
	off := r.off
	for len(stack) > 0 {
		if off >= len(r.buf) {
			return fmt.Errorf("%w: %s not closed", ErrTruncate, stack[len(stack)-1])
		}
		if r.buf[off]&typeMask == typeEnd {
			if err := r.checkEnd(r.buf[off], len(stack)); err != nil {
				return err
			}
			off++
			stack = stack[:len(stack)-1]
			continue
		}
		e, next, err := parse(r.buf, off, stack[len(stack)-1])
		if err != nil {
			return err
		}
		off = next
		if e.typ.IsContainer() {
			if len(r.stack)+len(stack)+1 > MaxDepth {
				return ErrTooDeep
			}
			stack = append(stack, e.typ)
		}
	}
	r.off = off
	return nil
}

type element struct {
	typ  Type
	tag  Tag
	size int
	val  []byte
}

// parse decodes the element at b[off], which isn't an end of container, inside a container of
// type parent (0 at the top level). It returns the offset after the element's head and value;
// a container's elements follow it.
func parse(b []byte, off int, parent Type) (element, int, error) {
	var e element
	ctl := b[off]
	off++

	var err error
	if e.tag, off, err = parseTag(b, off, ctl&tagMask); err != nil {
		return e, 0, err
	}
	switch {
	case parent == Structure && e.tag.Kind == AnonymousTag:
		return e, 0, fmt.Errorf("%w: anonymous element in a structure", ErrInvalid)
	case parent == Array && e.tag.Kind != AnonymousTag:
		return e, 0, fmt.Errorf("%w: tagged element %s in an array", ErrInvalid, e.tag)
	}

	code := ctl & typeMask
	switch {
	case code <= typeInt8+3, code >= typeUint8 && code <= typeUint8+3:
		e.typ = Int
		if code >= typeUint8 {
			e.typ, code = Uint, code-typeUint8
		}
		e.size = 1 << code
	case code == typeFalse || code == typeTrue:
		e.typ = Bool
		e.val = boolBytes[code-typeFalse : code-typeFalse+1]
		return e, off, nil
	case code == typeFloat32:
		e.typ, e.size = Float, 4
	case code == typeFloat64:
		e.typ, e.size = Float, 8
	case code >= typeUTF8Len1 && code <= typeBytesLen1+3:
		e.typ = UTF8
		if code >= typeBytesLen1 {
			e.typ, code = Bytes, code-typeBytesLen1
		} else {
			code -= typeUTF8Len1
		}
		e.size = 1 << code
		if len(b)-off < e.size {
			return e, 0, fmt.Errorf("%w: length of %s %s", ErrTruncate, e.typ, e.tag)
		}
		n := uintLE(b[off : off+e.size])
		off += e.size
		if n > uint64(len(b)-off) {
			return e, 0, fmt.Errorf("%w: %s %s is %d bytes, %d left", ErrTruncate, e.typ, e.tag, n, len(b)-off)
		}
		e.val = b[off : off+int(n)]
		return e, off + int(n), nil
	case code == typeNull:
		e.typ = Null
		return e, off, nil
	case code == typeStructure:
		e.typ = Structure
		return e, off, nil
	case code == typeArray:
		e.typ = Array
		return e, off, nil
	case code == typeList:
		e.typ = List
		return e, off, nil
	default:
		return e, 0, fmt.Errorf("%w: reserved element type 0x%02x", ErrInvalid, code)
	}
	if len(b)-off < e.size {
		return e, 0, fmt.Errorf("%w: %s %s", ErrTruncate, e.typ, e.tag)
	}
	e.val = b[off : off+e.size]
	return e, off + e.size, nil
}

var boolBytes = []byte{0, 1}

func parseTag(b []byte, off int, control byte) (Tag, int, error) {
	var width int
	switch control {
	case tagAnonymous:
		return Anonymous, off, nil
	case tagContext:
		width = 1
	case tagCommon2, tagImplicit2:
		width = 2
	case tagCommon4, tagImplicit4:
		width = 4
	case tagFullyQualify6:
		width = 6
	case tagFullyQualify8:
		width = 8
	}
	if len(b)-off < width {
		return Tag{}, 0, fmt.Errorf("%w: tag", ErrTruncate)
	}
	t := b[off : off+width]
	off += width
	switch control {
	case tagContext:
		return Context(t[0]), off, nil
	case tagCommon2, tagCommon4:
		return CommonProfile(uint32(uintLE(t))), off, nil
	case tagImplicit2, tagImplicit4:
		return ImplicitProfile(uint32(uintLE(t))), off, nil
	default:
		vendor := binary.LittleEndian.Uint16(t)
		profile := binary.LittleEndian.Uint16(t[2:])
		return FullyQualified(vendor, profile, uint32(uintLE(t[4:]))), off, nil
	}
}

// uintLE decodes a 1, 2, 4 or 8 byte little-endian unsigned integer.
func uintLE(b []byte) uint64 {
	var v uint64
	for _, v0 := range slices.Backward(b) {
		v = v<<8 | uint64(v0)
	}
	return v
}
