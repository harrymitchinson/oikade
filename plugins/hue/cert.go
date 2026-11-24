package hue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

// hueRootCA is the authorities that sign a bridge's certificate for its bridge ID: Philips Hue's
// root-bridge, valid until 2038-01-19, and Signify's Hue Root CA 01, valid until 2050-12-31.
//
//go:embed hue-root-ca.crt
var hueRootCA []byte

var hueRoots = func() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(hueRootCA) {
		panic("hue: the embedded root CAs are not PEM certificates")
	}
	return pool
}()

// certError is a bridge certificate the plugin doesn't trust. Its reason follows "a certificate
// that" in the plugin's status.
type certError struct{ reason string }

func (e *certError) Error() string { return e.reason }

// bridgeTLS checks a bridge's certificate against roots, and that its CN is the bridge's ID, or any
// bridge's for "". It checks the name itself, as Go's own check reads only a certificate's SANs, and
// a bridge's has none.
func bridgeTLS(roots *x509.CertPool, bridge string) *tls.Config {
	config := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks it.
	}
	config.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return &certError{"is missing"}
		}
		leaf := cs.PeerCertificates[0]
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		name := leaf.Subject.CommonName
		if name == "" {
			name = "no name"
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			CurrentTime: now(config),
		}); err != nil {
			var invalid x509.CertificateInvalidError
			if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
				return &certError{fmt.Sprintf("for %s is out of date: check this host's clock (%v)", name, err)}
			}
			return &certError{fmt.Sprintf("for %s isn't signed by Philips Hue: check the Address (%v)", name, err)}
		}
		switch {
		case bridge == "" && !bridgeID(leaf.Subject.CommonName):
			return &certError{fmt.Sprintf("is for %s, which isn't a Hue bridge's ID", name)}
		case bridge != "" && !strings.EqualFold(leaf.Subject.CommonName, bridge):
			return &certError{fmt.Sprintf("is for the bridge %s, not %s", name, bridge)}
		}
		return nil
	}
	return config
}

// identify is the ID and model of the bridge at address, from the certificate Philips Hue signed for
// it.
func identify(ctx context.Context, address string, roots *x509.CertPool) (found, error) {
	raw, err := dialBridge(ctx, "tcp", address)
	if err != nil {
		return found{}, err
	}
	conn := tls.Client(raw, bridgeTLS(roots, ""))
	defer func() { _ = conn.Close() }()
	if err := conn.HandshakeContext(ctx); err != nil {
		return found{}, err
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	f := found{id: strings.ToUpper(leaf.Subject.CommonName), address: address}
	if ou := leaf.Subject.OrganizationalUnit; len(ou) > 0 {
		f.model = ou[0]
	}
	return f, nil
}

// certFailure is the bridge's certificate the plugin doesn't trust in err, or nil.
func certFailure(err error) *certError {
	if e, ok := errors.AsType[*certError](err); ok {
		return e
	}
	return nil
}

// now is the config's time, as the TLS handshake uses it.
func now(config *tls.Config) time.Time {
	if config.Time != nil {
		return config.Time()
	}
	return time.Now()
}
