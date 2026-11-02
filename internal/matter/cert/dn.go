package cert

import (
	"encoding/asn1"
	"fmt"
	"unicode/utf8"
)

// AttributeType is a distinguished name attribute, numbered as in Matter's TLV form (Core spec,
// 6.5.6.1).
type AttributeType uint8

// Attribute types. The first sixteen are X.520's; the rest are Matter's own, which hold numbers.
const (
	AttrCommonName AttributeType = 1 + iota
	AttrSurname
	AttrSerialNumber
	AttrCountryName
	AttrLocalityName
	AttrStateOrProvinceName
	AttrOrganizationName
	AttrOrganizationalUnitName
	AttrTitle
	AttrName
	AttrGivenName
	AttrInitials
	AttrGenerationQualifier
	AttrDNQualifier
	AttrPseudonym
	AttrDomainComponent
	AttrNodeID
	AttrFirmwareSigningID
	AttrICACID
	AttrRCACID
	AttrFabricID
	AttrCASEAuthTag
	AttrVIDVerificationSignerID
)

// printableFlag marks, in an attribute's TLV tag, an X.520 string that is a PrintableString in
// X.509 rather than a UTF8String.
const printableFlag = 0x80

// attributeOIDs holds each attribute type's X.509 object identifier.
var attributeOIDs = map[AttributeType]asn1.ObjectIdentifier{
	AttrCommonName:              {2, 5, 4, 3},
	AttrSurname:                 {2, 5, 4, 4},
	AttrSerialNumber:            {2, 5, 4, 5},
	AttrCountryName:             {2, 5, 4, 6},
	AttrLocalityName:            {2, 5, 4, 7},
	AttrStateOrProvinceName:     {2, 5, 4, 8},
	AttrOrganizationName:        {2, 5, 4, 10},
	AttrOrganizationalUnitName:  {2, 5, 4, 11},
	AttrTitle:                   {2, 5, 4, 12},
	AttrName:                    {2, 5, 4, 41},
	AttrGivenName:               {2, 5, 4, 42},
	AttrInitials:                {2, 5, 4, 43},
	AttrGenerationQualifier:     {2, 5, 4, 44},
	AttrDNQualifier:             {2, 5, 4, 46},
	AttrPseudonym:               {2, 5, 4, 65},
	AttrDomainComponent:         {0, 9, 2342, 19200300, 100, 1, 25},
	AttrNodeID:                  {1, 3, 6, 1, 4, 1, 37244, 1, 1},
	AttrFirmwareSigningID:       {1, 3, 6, 1, 4, 1, 37244, 1, 2},
	AttrICACID:                  {1, 3, 6, 1, 4, 1, 37244, 1, 3},
	AttrRCACID:                  {1, 3, 6, 1, 4, 1, 37244, 1, 4},
	AttrFabricID:                {1, 3, 6, 1, 4, 1, 37244, 1, 5},
	AttrCASEAuthTag:             {1, 3, 6, 1, 4, 1, 37244, 1, 6},
	AttrVIDVerificationSignerID: {1, 3, 6, 1, 4, 1, 37244, 1, 7},
}

// IsMatter reports whether the attribute is one of Matter's own, which hold a number in Value
// rather than a string in Text.
func (t AttributeType) IsMatter() bool { return t >= AttrNodeID && t <= AttrVIDVerificationSignerID }

// hexDigits is how many hex digits the attribute's X.509 form has: Matter writes its numbers as
// fixed-width upper-case hex strings.
func (t AttributeType) hexDigits() int {
	if t == AttrCASEAuthTag {
		return 8
	}
	return 16
}

func (t AttributeType) valid() bool { return t >= AttrCommonName && t <= AttrVIDVerificationSignerID }

func (t AttributeType) String() string {
	if t.valid() {
		return attributeOIDs[t].String()
	}
	return fmt.Sprintf("AttributeType(%d)", uint8(t))
}

// Attribute is one attribute of a distinguished name. Matter holds one attribute in each relative
// distinguished name.
type Attribute struct {
	Type AttributeType
	// Value is a Matter attribute's number. CASE Authenticated Tags are 32 bits; the rest are 64.
	Value uint64
	// Text is an X.520 attribute's string.
	Text string
	// Printable says Text is a PrintableString in X.509 rather than a UTF8String. A domain
	// component is always an IA5String, and isn't Printable.
	Printable bool
}

func (a Attribute) check() error {
	switch {
	case !a.Type.valid():
		return invalid("attribute type %d", a.Type)
	case a.Type.IsMatter() && (a.Text != "" || a.Printable):
		return invalid("Matter attribute %s has a string", a.Type)
	case a.Type == AttrCASEAuthTag && a.Value > 0xFFFF_FFFF:
		return invalid("CASE Authenticated Tag %#x is wider than 32 bits", a.Value)
	case !a.Type.IsMatter() && a.Value != 0:
		return invalid("attribute %s has a number", a.Type)
	case a.Type == AttrDomainComponent && a.Printable:
		return invalid("domain component marked printable")
	case !utf8.ValidString(a.Text):
		return invalid("attribute %s isn't UTF-8", a.Type)
	}
	return nil
}

// x509Value returns the attribute's value as X.509 holds it: its DER string tag and contents.
func (a Attribute) x509Value() (byte, string) {
	switch {
	case a.Type.IsMatter():
		return tagUTF8String, fmt.Sprintf("%0*X", a.Type.hexDigits(), a.Value)
	case a.Type == AttrDomainComponent:
		return tagIA5String, a.Text
	case a.Printable:
		return tagPrintableString, a.Text
	}
	return tagUTF8String, a.Text
}

// parseUpperHex parses a Matter attribute's X.509 string: exactly n upper-case hex digits.
func parseUpperHex(s string, n int) (uint64, bool) {
	if len(s) != n {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint64(c-'0')
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint64(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// DN is a distinguished name, its attributes in order.
type DN []Attribute

// Get returns the value of the first Matter attribute of type t, and false if there is none or t
// isn't a Matter attribute.
func (d DN) Get(t AttributeType) (uint64, bool) {
	if !t.IsMatter() {
		return 0, false
	}
	for _, a := range d {
		if a.Type == t {
			return a.Value, true
		}
	}
	return 0, false
}

// Equal reports whether d and o are the same name, attribute for attribute.
func (d DN) Equal(o DN) bool {
	if len(d) != len(o) {
		return false
	}
	for i := range d {
		if d[i] != o[i] {
			return false
		}
	}
	return true
}
