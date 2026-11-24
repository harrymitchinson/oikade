package hue

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

// hueRootCA is Philips Hue's root-bridge authority, which signs every bridge's certificate for its
// bridge ID. It's valid until 2038-01-19.
//
//go:embed hue-root-ca.crt
var hueRootCA []byte

var hueRoots = func() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(hueRootCA) {
		panic("hue: the embedded root CA is not a PEM certificate")
	}
	return pool
}()

// certError is a bridge certificate the plugin doesn't trust. Its reason follows "a certificate
// that" in the plugin's status.
type certError struct{ reason string }

func (e *certError) Error() string { return e.reason }

// bridgeTLS checks a bridge's certificate against roots, and that its CN is the bridge's ID. It
// checks the name itself, as Go's own check reads only a certificate's SANs, and a bridge's has none.
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
		if !strings.EqualFold(leaf.Subject.CommonName, bridge) {
			return &certError{fmt.Sprintf("is for the bridge %s, not %s", name, bridge)}
		}
		return nil
	}
	return config
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
