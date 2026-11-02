package fabric

import (
	"bytes"

	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
)

// CASE returns the table as a CASE responder takes it.
func (t *Table) CASE() securechannel.FabricTable { return caseFabrics{t} }

type caseFabrics struct{ t *Table }

// Fabrics returns the fabrics, a pending one included, for a CASE handshake. A fabric that hasn't
// changed since the last call comes back as the same *securechannel.Fabric, and one that has as
// a new one: CASE caches what it derives by pointer.
func (c caseFabrics) Fabrics() []*securechannel.Fabric {
	t := c.t
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.current()
	cache := make(map[uint8]*securechannel.Fabric, len(current))
	out := make([]*securechannel.Fabric, 0, len(current))
	for _, f := range current {
		sf := t.caseCache[f.Index]
		if sf == nil || !sameCASE(sf, f) {
			sf = &securechannel.Fabric{
				Index: f.Index, RCAC: f.RCAC, ICAC: f.ICAC, NOC: f.NOC, Key: f.Key, IPK: f.IPK,
			}
		}
		cache[f.Index] = sf
		out = append(out, sf)
	}
	t.caseCache = cache
	return out
}

func sameCASE(sf *securechannel.Fabric, f *Fabric) bool {
	return sf.Key == f.Key && bytes.Equal(sf.RCAC, f.RCAC) && bytes.Equal(sf.ICAC, f.ICAC) &&
		bytes.Equal(sf.NOC, f.NOC) && bytes.Equal(sf.IPK, f.IPK)
}
