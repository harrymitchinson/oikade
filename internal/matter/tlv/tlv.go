// Package tlv encodes and decodes Matter TLV, the binary format that Matter messages, the
// Interaction Model and operational certificates are written in (Matter Core Specification,
// Appendix A).
//
// An element is a control byte, a tag and a value. The control byte's top three bits say how the
// tag is encoded, and its low five bits give the element type. Every multi-byte field is little
// endian. Containers (structures, arrays and lists) hold elements and end with an
// end-of-container element.
package tlv

import (
	"errors"
	"fmt"
)

// Type is an element's type, with the widths folded away: a 1-byte and an 8-byte signed integer
// are both Int.
type Type uint8

// Element types.
const (
	Int Type = iota + 1
	Uint
	Bool
	Float
	UTF8
	Bytes
	Null
	Structure
	Array
	List
)

func (t Type) String() string {
	switch t {
	case Int:
		return "int"
	case Uint:
		return "uint"
	case Bool:
		return "bool"
	case Float:
		return "float"
	case UTF8:
		return "utf8"
	case Bytes:
		return "bytes"
	case Null:
		return "null"
	case Structure:
		return "structure"
	case Array:
		return "array"
	case List:
		return "list"
	}
	return fmt.Sprintf("Type(%d)", uint8(t))
}

// IsContainer reports whether elements of type t hold other elements.
func (t Type) IsContainer() bool { return t == Structure || t == Array || t == List }

// Element type codes, the control byte's low five bits.
const (
	typeInt8      = 0x00 // through 0x03: 1, 2, 4 and 8 bytes
	typeUint8     = 0x04 // through 0x07
	typeFalse     = 0x08
	typeTrue      = 0x09
	typeFloat32   = 0x0A
	typeFloat64   = 0x0B
	typeUTF8Len1  = 0x0C // through 0x0F: a 1, 2, 4 or 8 byte length
	typeBytesLen1 = 0x10 // through 0x13
	typeNull      = 0x14
	typeStructure = 0x15
	typeArray     = 0x16
	typeList      = 0x17
	typeEnd       = 0x18
	typeMask      = 0x1F
)

// TagKind says how an element is labelled.
type TagKind uint8

// Tag kinds. Tags in the Matter common profile and in the implicit profile have a number only;
// fully qualified tags carry their vendor and profile too.
const (
	AnonymousTag TagKind = iota
	ContextTag
	CommonProfileTag
	ImplicitProfileTag
	FullyQualifiedTag
)

// Tag control codes, the control byte's top three bits.
const (
	tagAnonymous     = 0 << 5
	tagContext       = 1 << 5
	tagCommon2       = 2 << 5
	tagCommon4       = 3 << 5
	tagImplicit2     = 4 << 5
	tagImplicit4     = 5 << 5
	tagFullyQualify6 = 6 << 5
	tagFullyQualify8 = 7 << 5
	tagMask          = 7 << 5
)

// Tag labels an element. The zero Tag is anonymous.
type Tag struct {
	Kind    TagKind
	Vendor  uint16 // fully qualified tags only
	Profile uint16 // fully qualified tags only
	Number  uint32 // at most 255 for a context tag
}

// Anonymous is the tag of an element with no label, as in an array.
var Anonymous = Tag{}

// Context returns the context-specific tag n, the usual label of a field in a structure.
func Context(n uint8) Tag { return Tag{Kind: ContextTag, Number: uint32(n)} }

// CommonProfile returns tag n in the Matter common profile.
func CommonProfile(n uint32) Tag { return Tag{Kind: CommonProfileTag, Number: n} }

// ImplicitProfile returns tag n in the profile the surrounding protocol implies.
func ImplicitProfile(n uint32) Tag { return Tag{Kind: ImplicitProfileTag, Number: n} }

// FullyQualified returns tag n in the given vendor's profile.
func FullyQualified(vendor, profile uint16, n uint32) Tag {
	return Tag{Kind: FullyQualifiedTag, Vendor: vendor, Profile: profile, Number: n}
}

func (t Tag) String() string {
	switch t.Kind {
	case AnonymousTag:
		return "anonymous"
	case ContextTag:
		return fmt.Sprintf("%d", t.Number)
	case CommonProfileTag:
		return fmt.Sprintf("Matter::%d", t.Number)
	case ImplicitProfileTag:
		return fmt.Sprintf("::%d", t.Number)
	case FullyQualifiedTag:
		return fmt.Sprintf("%d::%d:%d", t.Vendor, t.Profile, t.Number)
	}
	return fmt.Sprintf("Tag(%d)", uint8(t.Kind))
}

// MaxDepth is how deeply containers may nest. Nothing in Matter comes close; the limit stops a
// hostile message from making a reader do unbounded work.
const MaxDepth = 32

// Decoding errors. Every error a Reader returns wraps ErrInvalid, except io.EOF at the end of a
// container or of the input.
var (
	ErrInvalid  = errors.New("invalid TLV")
	ErrTooDeep  = fmt.Errorf("%w: containers nested more than %d deep", ErrInvalid, MaxDepth)
	ErrTruncate = fmt.Errorf("%w: truncated", ErrInvalid)
)

// ErrType is returned when an element is read as a type it isn't.
type ErrType struct {
	Want, Got Type
}

func (e *ErrType) Error() string {
	return fmt.Sprintf("invalid TLV: element is %s, not %s", e.Got, e.Want)
}

// Unwrap makes an ErrType an ErrInvalid: a message with the wrong type in a field is invalid.
func (e *ErrType) Unwrap() error { return ErrInvalid }
