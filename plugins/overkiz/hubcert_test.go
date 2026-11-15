package overkiz

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// The fake hub's PIN, and the name its certificate gives.
const (
	hubPIN  = "1234-5678-9012"
	hubName = "gateway-1234-5678-9012.local"
)

// authority is a test CA, standing in for Overkiz's.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{cert: cert, key: key}
}

func (a authority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// issue signs a server certificate with the CN cn and the DNS SANs dns.
func (a authority) issue(t *testing.T, cn string, dns []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: dns,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newHubServer serves handler over TLS with a certificate for cn and dns, from an authority of its
// own, and returns that authority.
func newHubServer(t *testing.T, handler http.Handler, cn string, dns []string) (*x509.CertPool, *httptest.Server) {
	t.Helper()
	ca := newAuthority(t)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issue(t, cn, dns)}}
	// The handshakes the plugin refuses aren't the test's output.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return ca.pool(), server
}

func TestTheEmbeddedRootIsOverkizs(t *testing.T) {
	block, _ := pem.Decode(overkizRootCA)
	if block == nil {
		t.Fatal("the embedded root CA isn't PEM")
	}
	// The SHA-256 fingerprint of https://ca.overkiz.com/overkiz-root-ca-2048.crt.
	const want = "bbfa96111eae15e3c6ed15f343276c0c2726032ea19df5bbd067e2f1cffc44db"
	if sum := sha256.Sum256(block.Bytes); hex.EncodeToString(sum[:]) != want {
		t.Errorf("the embedded root CA's fingerprint is %x", sum)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "Overkiz Root CA" || !cert.IsCA {
		t.Errorf("the embedded root CA is %s", cert.Subject)
	}
}

func TestGatewayPIN(t *testing.T) {
	for address, want := range map[string]string{
		"gateway-1234-5678-9012.local:8443":  "1234-5678-9012",
		"gateway-1234-5678-9012.local":       "1234-5678-9012",
		"Gateway-1234-5678-9012.LOCAL.:8443": "1234-5678-9012",
		"192.168.1.40:8443":                  "",
		"gateway-12-34.local:8443":           "",
		"tahoma.example.com:8443":            "",
	} {
		if got := gatewayPIN(address); got != want {
			t.Errorf("gatewayPIN(%q) is %q, want %q", address, got, want)
		}
	}
}

func TestAPINIsChecked(t *testing.T) {
	for _, l := range []Local{
		{Address: "192.168.1.40:8443", PIN: "1234"},
		{Address: "gateway-1234-5678-9012.local:8443", PIN: "1111-2222-3333"},
	} {
		if err := l.check(); err == nil {
			t.Errorf("%+v was taken", l)
		}
	}
	for _, l := range []Local{
		{Address: "192.168.1.40:8443", PIN: "1234-5678-9012"},
		{Address: "gateway-1234-5678-9012.local:8443", PIN: "1234-5678-9012"},
		{Address: "192.168.1.40:8443"},
	} {
		if err := l.check(); err != nil {
			t.Errorf("%+v was refused: %v", l, err)
		}
	}
}

// verify runs hubTLS's check on a certificate from ca for cn and dns.
func verify(t *testing.T, roots *x509.CertPool, ca authority, pin, cn string, dns ...string) error {
	t.Helper()
	leaf, err := x509.ParseCertificate(ca.issue(t, cn, dns).Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return hubTLS(roots, pin).VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}})
}

func TestTheHubsCertificate(t *testing.T) {
	ca := newAuthority(t)
	for _, c := range []struct {
		name, pin, cn string
		dns           []string
		roots         *x509.CertPool
		refused       string
	}{
		{name: "named in its CN", pin: hubPIN, cn: hubName},
		{name: "named in a SAN", pin: hubPIN, cn: "TaHoma", dns: []string{hubName}},
		{name: "in another case", pin: hubPIN, cn: strings.ToUpper(hubName)},
		{name: "with no PIN to check", cn: "gateway-9999-9999-9999.local"},
		{name: "another hub's", pin: hubPIN, cn: "gateway-9999-9999-9999.local", refused: "is for gateway-9999-9999-9999.local, not " + hubName},
		{name: "not Overkiz's", pin: hubPIN, cn: hubName, roots: overkizRoots, refused: "isn't signed by Overkiz's root CA"},
		{name: "not Overkiz's, with no PIN", cn: hubName, roots: overkizRoots, refused: "isn't signed by Overkiz's root CA"},
	} {
		roots := c.roots
		if roots == nil {
			roots = ca.pool()
		}
		err := verify(t, roots, ca, c.pin, c.cn, c.dns...)
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s: %v, want it refused as %q", c.name, err, c.refused)
		}
	}
	if err := hubTLS(ca.pool(), hubPIN).VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Error("a hub with no certificate was taken")
	}
}

func TestAnotherHubsCertificateIsRefusedBeforeTheTokenIsSent(t *testing.T) {
	hub := newFakeHub(t)
	account := Local{Address: hub.address(), TokenSecret: "tahoma-token", PIN: "9999-9999-9999", roots: hub.ca}
	b := startWith(t, hub, account, fast, token, kitchen)
	b.eventually("the refusal", func() bool {
		return strings.Contains(b.status().Error, "the hub's certificate is for "+hubName+", not gateway-9999-9999-9999.local")
	})
	// It has never seen the hub's list, so the device on record isn't found.
	if d := b.devices(); len(d.Devices) != 1 || d.Devices[0].Found {
		t.Errorf("devices from a hub it doesn't trust: %+v", d)
	}
	if n := b.logs.count("Refused Overkiz hub's certificate"); n == 0 {
		t.Error("the refusal wasn't logged")
	}
	if s := b.status(); s.State != contractv1.Online {
		t.Errorf("the plugin went %s", s.State)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.requests) != 0 {
		t.Errorf("the plugin sent requests to a hub it doesn't trust: %v", hub.requests)
	}
}

func TestAHubNotSignedByOverkizIsRefused(t *testing.T) {
	hub := newFakeHub(t)
	// With no roots of its own, the plugin trusts only Overkiz's.
	b := startWith(t, hub, Local{Address: hub.address(), TokenSecret: "tahoma-token"}, fast, token)
	b.eventually("the refusal", func() bool { return strings.Contains(b.status().Error, "isn't signed by Overkiz's root CA") })
}

func TestTheHubsPINComesFromItsNameOrPIN(t *testing.T) {
	for _, c := range []struct {
		l    Local
		want string
	}{
		{Local{Address: "gateway-1234-5678-9012.local:8443"}, hubPIN},
		{Local{Address: "192.168.1.40:8443", PIN: hubPIN}, hubPIN},
		{Local{Address: "192.168.1.40:8443"}, ""},
	} {
		if got := c.l.pin(); got != c.want {
			t.Errorf("the PIN of %+v is %q, want %q", c.l, got, c.want)
		}
	}
}
