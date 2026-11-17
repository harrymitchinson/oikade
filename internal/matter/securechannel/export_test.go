package securechannel

import (
	"crypto/ecdh"
	"testing"
)

// For TestCaptureChipToolCASE, in package securechannel_test because it runs a whole node, which
// imports this package.
type (
	Capture        = capture
	CapturedFabric = capturedFabric
	Datagram       = datagram
	HexBytes       = hexBytes
)

// Write writes the capture to testdata/name.
func (c *capture) Write(name string) error { return c.write(name) }

// ChipToolLog is the lines of chip-tool's output that say what it sent and received.
func ChipToolLog(out string) []string { return chipToolLog(out) }

// KeepEphemeralKeys has CASE call keep with each ephemeral key it draws, until t ends.
func KeepEphemeralKeys(t *testing.T, keep func(*ecdh.PrivateKey)) {
	original := ephemeralKey
	t.Cleanup(func() { ephemeralKey = original })
	ephemeralKey = func() (*ecdh.PrivateKey, error) {
		k, err := original()
		if err == nil {
			keep(k)
		}
		return k, err
	}
}
