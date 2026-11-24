package hue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"
)

// handshake is what bridgeTLS says of a bridge that presents cert.
func handshake(t *testing.T, cert tls.Certificate, config *tls.Config) error {
	t.Helper()
	// A socket, not net.Pipe, whose writes wait for a read: a refusal is written mid-handshake.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		server, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = server.Close() }()
		_ = tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}}).HandshakeContext(t.Context())
	}()
	ctx, cancel := context.WithTimeout(t.Context(), waitLimit)
	defer cancel()
	client, err := (&net.Dialer{}).DialContext(ctx, "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	return tls.Client(client, config).HandshakeContext(ctx)
}

func TestTheBridgesCertificateIsChecked(t *testing.T) {
	ca := newAuthority(t)
	hour := time.Now().Add(time.Hour)
	valid := ca.issue(t, bridgeOne, time.Now().Add(-time.Hour), hour)
	for _, c := range []struct {
		name   string
		cert   tls.Certificate
		roots  *x509.CertPool
		bridge string
		want   string
	}{
		{"its own", valid, ca.pool, bridgeOne, ""},
		{"its own, in lower case", valid, ca.pool, strings.ToLower(bridgeOne), ""},
		{"another bridge's", ca.issue(t, bridgeTwo, time.Now().Add(-time.Hour), hour), ca.pool, bridgeOne,
			"is for the bridge " + bridgeTwo + ", not " + bridgeOne},
		{"another authority's", valid, newAuthority(t).pool, bridgeOne, "for " + bridgeOne + " isn't signed by Philips Hue"},
		{"out of date", ca.issue(t, bridgeOne, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)), ca.pool, bridgeOne,
			"for " + bridgeOne + " is out of date: check this host's clock"},
		{"with no name", ca.issue(t, "", time.Now().Add(-time.Hour), hour), ca.pool, bridgeOne, "is for the bridge no name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := handshake(t, c.cert, bridgeTLS(c.roots, c.bridge))
			switch {
			case c.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.want != "" && (certFailure(err) == nil || !strings.Contains(certFailure(err).reason, c.want)):
				t.Errorf("got %v, want a refusal that says %q", err, c.want)
			}
		})
	}
}

func TestTheEmbeddedRootIsPhilipsHues(t *testing.T) {
	block, _ := pem.Decode(hueRootCA)
	if block == nil {
		t.Fatal("the root isn't PEM")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// The key ID a Hue Bridge Pro's certificate names as its issuer's.
	if got, want := strings.ToUpper(hex.EncodeToString(root.SubjectKeyId)), "67638D4C5AC34FAA3C22D46729FA962A71C51A64"; got != want {
		t.Errorf("the root's key ID is %s, want %s", got, want)
	}
	if root.Subject.CommonName != "root-bridge" || !root.IsCA {
		t.Errorf("the root is %s, want Philips Hue's root-bridge authority", root.Subject)
	}
}
