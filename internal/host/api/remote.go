package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"

	"oikade.dev/internal/mdns"
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

// Dial implements Transport. A host without the pin is refused before anything is sent. A host
// named in .local is found over mDNS, which Go's own resolver doesn't do on Linux.
func (r Remote) Dial(ctx context.Context) (net.Conn, error) {
	if r.Pin == "" {
		return nil, errors.New("no pin for the host")
	}
	raw, err := mdns.DialContext(ctx, "tcp", r.Address)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, PinnedTLS(r.Pin))
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

func (r Remote) token() string { return r.Token }

// authorizer is a Transport whose calls carry a token.
type authorizer interface{ token() string }

// ErrOtherHost is a host whose key doesn't have the pin it was reached by.
var ErrOtherHost = errors.New("it isn't the host you logged in to")

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
				return fmt.Errorf("the host's pin is %s, not %s: %w", got, pin, ErrOtherHost)
			}
			return nil
		},
	}
}
