package mqtt

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"

	"github.com/harrymitchinson/oikade/internal/mdns"
)

// dialLocal dials a broker named in .local, which Go's own resolver can't find in a binary built
// without cgo, through mdns.DialContext (OIK-328). autopaho calls it in place of its own dialing,
// for every scheme, so it does TLS itself. It can't do WebSockets, which autopaho dials with a
// library of its own.
func dialLocal(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(context.Context, autopaho.ClientConfig, *url.URL) (net.Conn, error) {
	return func(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
		// autopaho hands this the connection's context, not one bounded by its ConnectTimeout.
		ctx, cancel := context.WithTimeout(ctx, cmpTimeout(cfg.ConnectTimeout))
		defer cancel()
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "mqtt", "tcp", "", "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
		default:
			return nil, fmt.Errorf("can't reach %s over %s: Oikade finds a .local broker only over mqtt:// or mqtts://, so use one of those or its IP address", u.Hostname(), scheme)
		}
		conn, err := dial(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		switch scheme {
		case "mqtt", "tcp", "":
			return conn, nil
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

// cmpTimeout is autopaho's ConnectTimeout, or its default if it's unset.
func cmpTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}
