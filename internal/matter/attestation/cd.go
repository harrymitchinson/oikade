package attestation

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"fmt"
	"io"
	"slices"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// Declaration is a Certification Declaration's content (Core spec, 6.3.1).
type Declaration struct {
	VendorID          uint16
	ProductIDs        []uint16
	DeviceTypeID      uint32
	CertificateID     string // 19 characters
	SecurityLevel     uint8
	SecurityInfo      uint16
	VersionNumber     uint16
	CertificationType uint8 // 0 for development and test
}

func (d *Declaration) encode() []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutUint(tlv.Context(0), 1) // format_version
	w.PutUint(tlv.Context(1), uint64(d.VendorID))
	w.StartArray(tlv.Context(2))
	for _, p := range d.ProductIDs {
		w.PutUint(tlv.Anonymous, uint64(p))
	}
	w.End()
	w.PutUint(tlv.Context(3), uint64(d.DeviceTypeID))
	w.PutUTF8(tlv.Context(4), d.CertificateID)
	w.PutUint(tlv.Context(5), uint64(d.SecurityLevel))
	w.PutUint(tlv.Context(6), uint64(d.SecurityInfo))
	w.PutUint(tlv.Context(7), uint64(d.VersionNumber))
	w.PutUint(tlv.Context(8), uint64(d.CertificationType))
	w.End()
	return w.Bytes()
}

func decodeDeclaration(b []byte) (*Declaration, error) {
	d := &Declaration{}
	r := tlv.NewReader(b)
	if err := r.Next(); err != nil || r.Type() != tlv.Structure {
		return nil, fmt.Errorf("%w: CD content isn't a structure", ErrInvalid)
	}
	if err := r.Enter(); err != nil {
		return nil, err
	}
	var seen uint16
	for {
		err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: CD content: %w", ErrInvalid, err)
		}
		tag := r.Tag()
		if tag.Kind != tlv.ContextTag {
			continue
		}
		seen |= 1 << tag.Number
		var v uint64
		switch tag.Number {
		case 1, 3, 5, 6, 7, 8:
			v, err = r.Uint()
		}
		switch tag.Number {
		case 1:
			d.VendorID = uint16(v)
		case 2:
			if err = r.Enter(); err != nil {
				break
			}
			for r.Next() == nil {
				p, perr := r.Uint()
				if perr != nil {
					return nil, fmt.Errorf("%w: CD product ID: %w", ErrInvalid, perr)
				}
				d.ProductIDs = append(d.ProductIDs, uint16(p))
			}
			err = r.Exit()
		case 3:
			d.DeviceTypeID = uint32(v)
		case 4:
			d.CertificateID, err = r.UTF8()
		case 5:
			d.SecurityLevel = uint8(v)
		case 6:
			d.SecurityInfo = uint16(v)
		case 7:
			d.VersionNumber = uint16(v)
		case 8:
			d.CertificationType = uint8(v)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: CD field %d: %w", ErrInvalid, tag.Number, err)
		}
	}
	if want := uint16(1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<8); seen&want != want {
		return nil, fmt.Errorf("%w: CD is missing a field", ErrInvalid)
	}
	return d, nil
}

// CMS object identifiers.
var (
	oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidData       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSHA256     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidECDSA256   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
)

// The CMS SignedData a CD is wrapped in (RFC 5652), as connectedhomeip encodes it: the signer
// named by its subject key ID, and no signed attributes, so the signature covers the content.
type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     signedData `asn1:"explicit,tag:0"`
}

type signedData struct {
	Version          int
	DigestAlgorithms []algorithm `asn1:"set"`
	EncapContent     encapContent
	SignerInfos      []signerInfo `asn1:"set"`
}

type algorithm struct {
	Algorithm asn1.ObjectIdentifier
}

type encapContent struct {
	ContentType asn1.ObjectIdentifier
	Content     []byte `asn1:"explicit,tag:0"`
}

type signerInfo struct {
	Version            int
	SubjectKeyID       []byte `asn1:"tag:0"`
	DigestAlgorithm    algorithm
	SignatureAlgorithm algorithm
	Signature          []byte
}

// SignCD encodes d and signs it with key, whose certificate has subject key ID skid.
func SignCD(d Declaration, key crypto.Signer, skid []byte) ([]byte, error) {
	content := d.encode()
	digest := sha256.Sum256(content)
	sig, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(contentInfo{
		ContentType: oidSignedData,
		Content: signedData{
			Version:          3,
			DigestAlgorithms: []algorithm{{oidSHA256}},
			EncapContent:     encapContent{ContentType: oidData, Content: content},
			SignerInfos: []signerInfo{{
				Version: 3, SubjectKeyID: skid, DigestAlgorithm: algorithm{oidSHA256},
				SignatureAlgorithm: algorithm{oidECDSA256}, Signature: sig,
			}},
		},
	})
}

// ParseCD checks a CD's signature with the signer its subject key ID names in signers, and
// returns its content.
func ParseCD(cd []byte, signers map[string]*ecdsa.PublicKey) (*Declaration, error) {
	var ci contentInfo
	if rest, err := asn1.Unmarshal(cd, &ci); err != nil || len(rest) != 0 {
		return nil, fmt.Errorf("%w: CD isn't CMS: %v", ErrInvalid, err)
	}
	sd := ci.Content
	if !ci.ContentType.Equal(oidSignedData) || !sd.EncapContent.ContentType.Equal(oidData) || len(sd.SignerInfos) != 1 {
		return nil, fmt.Errorf("%w: CD isn't SignedData with one signer", ErrInvalid)
	}
	si := sd.SignerInfos[0]
	if !si.DigestAlgorithm.Algorithm.Equal(oidSHA256) || !si.SignatureAlgorithm.Algorithm.Equal(oidECDSA256) {
		return nil, fmt.Errorf("%w: CD isn't signed with ECDSA and SHA-256", ErrInvalid)
	}
	pub := signers[string(si.SubjectKeyID)]
	if pub == nil {
		return nil, fmt.Errorf("%w: CD signer %x isn't trusted", ErrInvalid, si.SubjectKeyID)
	}
	digest := sha256.Sum256(sd.EncapContent.Content)
	if !ecdsa.VerifyASN1(pub, digest[:], si.Signature) {
		return nil, fmt.Errorf("%w: CD signature", ErrInvalid)
	}
	return decodeDeclaration(sd.EncapContent.Content)
}

// covers reports whether the CD covers a product.
func (d *Declaration) covers(vid, pid uint16) bool {
	return d.VendorID == vid && slices.Contains(d.ProductIDs, pid)
}

func asn1Unmarshal(b []byte, v any) ([]byte, error) {
	rest, err := asn1.Unmarshal(b, v)
	if err == nil && len(rest) != 0 {
		err = fmt.Errorf("trailing data")
	}
	return rest, err
}
