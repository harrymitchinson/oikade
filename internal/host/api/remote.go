package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
)

// DefaultPort is the network API's port when an address has none.
const DefaultPort = "8443"

// Address fills in the port of an address that has none.
func Address(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), DefaultPort)
}

// Remote is a host's network API, over TLS. It trusts only the host whose public key has Pin, with
// no CA and no name, so it works by IP address.
type Remote struct {
	// Address is the host and port.
	Address string
	Pin     string
	// Token is the client's, and empty for a login.
	Token string
}

// Dial implements Transport. A host without the pin is refused before anything is sent.
func (r Remote) Dial(ctx context.Context) (net.Conn, error) {
	if r.Pin == "" {
		return nil, errors.New("no pin for the host")
	}
	dialer := &tls.Dialer{Config: PinnedTLS(r.Pin)}
	return dialer.DialContext(ctx, "tcp", r.Address)
}

func (r Remote) token() string { return r.Token }

// authorizer is a Transport whose calls carry a token.
type authorizer interface{ token() string }

// PinnedTLS trusts only the certificate whose public key has pin.
func PinnedTLS(pin string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Pinned instead: the certificate is self-signed, and has no name worth checking.
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks the pin.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the host sent no certificate")
			}
			if got := Pin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo); got != pin {
				return fmt.Errorf("the host's pin is %s, not %s: it isn't the host you logged in to", got, pin)
			}
			return nil
		},
	}
}
