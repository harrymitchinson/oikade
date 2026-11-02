package fabric

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// formatVersion is the version of the encoded table. A table with another version doesn't load.
const formatVersion = 1

// The encoded table: JSON, with certificates in Matter TLV and keys in PKCS #8, each base64.
type stored struct {
	Version         int            `json:"version"`
	NextFabricIndex uint8          `json:"nextFabricIndex"`
	NextEndpoint    uint16         `json:"nextEndpoint"`
	Fabrics         []storedFabric `json:"fabrics"`
}

type storedFabric struct {
	Index          uint8       `json:"index"`
	RCAC           []byte      `json:"rcac"`
	ICAC           []byte      `json:"icac,omitempty"`
	NOC            []byte      `json:"noc"`
	OperationalKey []byte      `json:"operationalKey"`
	IPK            []byte      `json:"ipk"`
	VendorID       uint16      `json:"vendorID"`
	Label          string      `json:"label,omitempty"`
	ACL            []storedACE `json:"acl"`
}

type storedACE struct {
	Privilege uint8          `json:"privilege"`
	AuthMode  uint8          `json:"authMode"`
	Subjects  []uint64       `json:"subjects,omitempty"`
	Targets   []storedTarget `json:"targets,omitempty"`
}

type storedTarget struct {
	Cluster    *uint32 `json:"cluster,omitempty"`
	Endpoint   *uint16 `json:"endpoint,omitempty"`
	DeviceType *uint32 `json:"deviceType,omitempty"`
}

// encode encodes the committed state. Call it with the lock held.
func (t *Table) encode() ([]byte, error) {
	s := stored{Version: formatVersion, NextFabricIndex: t.nextIndex, NextEndpoint: t.nextEndpoint, Fabrics: []storedFabric{}}
	for _, f := range t.fabrics {
		key, err := x509.MarshalPKCS8PrivateKey(f.Key)
		if err != nil {
			return nil, fmt.Errorf("matter: encoding fabric %d's key: %w", f.Index, err)
		}
		sf := storedFabric{
			Index: f.Index, RCAC: f.RCAC, ICAC: f.ICAC, NOC: f.NOC, OperationalKey: key,
			IPK: f.IPK, VendorID: f.VendorID, Label: f.Label, ACL: []storedACE{},
		}
		for _, e := range f.ACL {
			se := storedACE{Privilege: uint8(e.Privilege), AuthMode: uint8(e.AuthMode), Subjects: e.Subjects}
			for _, tg := range e.Targets {
				se.Targets = append(se.Targets, storedTarget(tg))
			}
			sf.ACL = append(sf.ACL, se)
		}
		s.Fabrics = append(s.Fabrics, sf)
	}
	return json.MarshalIndent(s, "", "  ")
}

// decode replaces the table's state with b's. The certificates are checked, but not their
// validity periods: a stored fabric stays usable until it's removed or updated.
func (t *Table) decode(b []byte) error {
	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s.Version != formatVersion {
		return fmt.Errorf("unsupported version %d", s.Version)
	}
	var fabrics []Fabric
	for _, sf := range s.Fabrics {
		k, err := x509.ParsePKCS8PrivateKey(sf.OperationalKey)
		if err != nil {
			return fmt.Errorf("fabric %d's key: %w", sf.Index, err)
		}
		key, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return fmt.Errorf("fabric %d's key isn't ECDSA", sf.Index)
		}
		f := Fabric{
			Index: sf.Index, RCAC: sf.RCAC, ICAC: sf.ICAC, NOC: sf.NOC, Key: key,
			IPK: sf.IPK, VendorID: sf.VendorID, Label: sf.Label,
		}
		for _, se := range sf.ACL {
			e := ACE{Privilege: im.Privilege(se.Privilege), AuthMode: transport.AuthMode(se.AuthMode), Subjects: se.Subjects}
			for _, tg := range se.Targets {
				e.Targets = append(e.Targets, Target(tg))
			}
			f.ACL = append(f.ACL, e)
		}
		if err := f.derive(time.Time{}); err != nil {
			return fmt.Errorf("fabric %d: %w", sf.Index, err)
		}
		if f.Index == 0 || f.Index == 255 || slices.ContainsFunc(fabrics, func(o Fabric) bool { return o.Index == f.Index }) {
			return fmt.Errorf("invalid fabric index %d", f.Index)
		}
		fabrics = append(fabrics, f)
	}
	slices.SortFunc(fabrics, func(a, b Fabric) int { return int(a.Index) - int(b.Index) })
	t.fabrics, t.nextIndex, t.nextEndpoint = fabrics, max(s.NextFabricIndex, 1), s.NextEndpoint
	return nil
}

// DirStore keeps the table in a file, fabrics.json, in a directory.
type DirStore struct {
	Dir string
}

const storeFile = "fabrics.json"

// Load reads the file, or returns nil if there isn't one.
func (d DirStore) Load() ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(d.Dir, storeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Save writes the file atomically: to a temporary file, synced, then renamed over the old one.
func (d DirStore) Save(b []byte) (err error) {
	f, err := os.CreateTemp(d.Dir, storeFile+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(d.Dir, storeFile)); err != nil {
		return err
	}
	dir, err := os.Open(d.Dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
