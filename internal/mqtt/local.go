package mqtt

import (
	"cmp"
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"

	"oikade.dev/internal/mdns"
)

// The schemes dialLocal dials: autopaho's, but WebSockets, which it dials with a library of its own.
var (
	plainSchemes = []string{"mqtt", "tcp", ""}
	tlsSchemes   = []string{"ssl", "tls", "mqtts", "mqtt+ssl", "tcps"}
)

// findsLocal reports whether dialLocal should dial u: a broker named in .local, which Go's own
// resolver can't find in a binary built without cgo (OIK-328), over a scheme it can dial.
func findsLocal(u *url.URL) bool {
	scheme := strings.ToLower(u.Scheme)
	return mdns.IsLocal(u.Hostname()) && (slices.Contains(plainSchemes, scheme) || slices.Contains(tlsSchemes, scheme))
}

// dialLocal dials a broker through dial, in place of autopaho's own dialing, so it does TLS itself.
func dialLocal(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(context.Context, autopaho.ClientConfig, *url.URL) (net.Conn, error) {
	return func(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
		// autopaho hands this the connection's context, not one bounded by its ConnectTimeout.
		ctx, cancel := context.WithTimeout(ctx, cmp.Or(cfg.ConnectTimeout, 10*time.Second))
		defer cancel()
		conn, err := dial(ctx, "tcp", u.Host)
		if err != nil || slices.Contains(plainSchemes, strings.ToLower(u.Scheme)) {
			return conn, err
		}
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.TlsCfg != nil {
			config = cfg.TlsCfg.Clone()
		}
		if config.ServerName == "" {
			config.ServerName = u.Hostname()
		}
		tc := tls.Client(conn, config)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return packets.NewThreadSafeConn(tc), nil
	}
}

// localDialer is how a .local broker is dialed, which tests replace.
var localDialer = mdns.DialContext
