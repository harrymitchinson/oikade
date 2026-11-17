package crypto

import "testing"

// FuzzParse feeds the parsers of what a peer or a setup file sends: a SPAKE2+ share, which a
// commissioner sends in Pake1 before it has authenticated, and a serialized verifier. Neither may
// panic, and a point that parses encodes to the same bytes.
func FuzzParse(f *testing.F) {
	f.Add(pointM.bytes())
	f.Add(pointN.bytes())
	if v, err := NewVerifier(20202021, []byte("SPAKE2P Key Salt"), 1000); err == nil {
		f.Add(v.Bytes())
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if p, err := parsePoint(b); err == nil {
			if got := p.bytes(); string(got) != string(b) {
				t.Errorf("point %x encodes as %x", b, got)
			}
		}
		_, _ = ParseVerifier(b)
	})
}
