package securechannel

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// FuzzDecoders feeds each Secure Channel decoder the same bytes. Any host on the network reaches
// them before it has authenticated: PBKDFParamRequest, Pake1, Pake3, Sigma1 and status reports
// come unsecured, and the rest decode what a peer encrypted. None may panic. The seeds are every
// unsecured payload of the chip-tool captures in testdata.
func FuzzDecoders(f *testing.F) {
	for _, name := range []string{"pase-chip-tool.json", "case-chip-tool.json"} {
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		var c capture
		if err := json.Unmarshal(raw, &c); err != nil {
			f.Fatal(err)
		}
		for _, d := range c.Datagrams {
			h, n, err := transport.ParseHeader(d.Data)
			if err != nil || h.SessionID != 0 {
				continue
			}
			if _, m, err := transport.ParseProtocolHeader(d.Data[n:]); err == nil {
				f.Add([]byte(d.Data[n+m:]))
			}
		}
	}
	f.Add(StatusReport{General: 1, Protocol: transport.SecureChannel, Code: 2, Data: []byte{3}}.Message().Payload)
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodePBKDFParamRequest(b)
		_, _ = decodePBKDFParamResponse(b)
		// Pake1, Pake2 and Pake3, with the sizes PASE reads them with.
		_, _ = decodePake(b, mcrypto.PointSize)
		_, _ = decodePake(b, mcrypto.PointSize, sha256.Size)
		_, _ = decodePake(b, sha256.Size)
		_, _ = decodeSigma1(b)
		_, _ = decodeSigma2(b)
		_, _ = decodeSigma2Resume(b)
		_, _ = decodeSigma3(b)
		_, _ = decodeTBEData(b, false)
		_, _ = decodeTBEData(b, true)
		_, _ = ParseStatusReport(b)
	})
}
