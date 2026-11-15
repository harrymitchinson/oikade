package overkiz

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"
)

// overkizRootCA is Overkiz's own root authority, which signs every TaHoma hub's certificate. Somfy
// publishes it at https://ca.overkiz.com/overkiz-root-ca-2048.crt, and it's valid until 2036-04-27.
//
//go:embed overkiz-root-ca.crt
var overkizRootCA []byte

// overkizRoots holds overkizRootCA.
var overkizRoots = func() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(overkizRootCA) {
		panic("overkiz: the embedded root CA is not a PEM certificate")
	}
	return pool
}()

// pinPattern is a hub's PIN, such as 1234-5678-9012.
var pinPattern = regexp.MustCompile(`^[0-9A-Za-z]{4}-[0-9A-Za-z]{4}-[0-9A-Za-z]{4}$`)

// gatewayPIN is the PIN in an address of the form gateway-<pin>.local[:port], or "".
func gatewayPIN(address string) string {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	pin, ok := strings.CutPrefix(host, "gateway-")
	if !ok {
		return ""
	}
	pin, ok = strings.CutSuffix(pin, ".local")
	if !ok || !pinPattern.MatchString(pin) {
		return ""
	}
	return pin
}

// certError is a hub whose certificate the plugin doesn't trust.
type certError struct{ reason string }

func (e *certError) Error() string { return e.reason }

// hubTLS checks a hub's certificate against roots, and its name against gateway-<pin>.local if the
// PIN is known. It checks the names itself, as Go's own check ignores a certificate's CN, which is
// where a hub's certificate may name it. With no PIN, any hub's certificate Overkiz signed passes.
func hubTLS(roots *x509.CertPool, pin string) *tls.Config {
	config := &tls.Config{
		// VerifyConnection checks the certificate in place of Go's own check.
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks it.
	}
	config.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return &certError{"is missing: check the Address"}
		}
		leaf := cs.PeerCertificates[0]
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		names := certNames(leaf)
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			CurrentTime: now(config),
		}); err != nil {
			var invalid x509.CertificateInvalidError
			if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
				return &certError{fmt.Sprintf("for %s is out of date: check this host's clock (%v)", strings.Join(names, ", "), err)}
			}
			return &certError{fmt.Sprintf("for %s isn't signed by Overkiz's root CA: check the Address (%v)", strings.Join(names, ", "), err)}
		}
		if pin == "" {
			return nil
		}
		want := "gateway-" + strings.ToLower(pin) + ".local"
		if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, want) }) {
			return &certError{fmt.Sprintf("is for %s, not %s: check the PIN", strings.Join(names, ", "), want)}
		}
		return nil
	}
	return config
}

// certNames is every name a certificate gives: its CN and its DNS and IP SANs.
func certNames(c *x509.Certificate) []string {
	var names []string
	if c.Subject.CommonName != "" {
		names = append(names, c.Subject.CommonName)
	}
	names = append(names, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return []string{"no name"}
	}
	return names
}

// certFailure is the hub's certificate the plugin doesn't trust in err, or nil.
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
