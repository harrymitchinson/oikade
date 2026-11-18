package api

import (
	"context"
	"net"
	"path/filepath"
)

// Transport reaches a host's API.
type Transport interface {
	// Dial opens a connection to the API.
	Dial(ctx context.Context) (net.Conn, error)
}

// Local is a host on this machine.
type Local struct {
	// DataDir is the host's data directory.
	DataDir string
}

// Dial implements Transport.
func (l Local) Dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(l.DataDir, SocketName))
}

// Explained is t, with each error its Dial returns passed through explain first, as a command puts
// a host it can't reach in its user's words. A call over it carries t's token, if t has one.
func Explained(t Transport, explain func(error) error) Transport {
	return explained{t, explain}
}

type explained struct {
	Transport
	explain func(error) error
}

func (e explained) Dial(ctx context.Context) (net.Conn, error) {
	conn, err := e.Transport.Dial(ctx)
	if err != nil {
		return nil, e.explain(err)
	}
	return conn, nil
}

func (e explained) token() string {
	if a, ok := e.Transport.(authorizer); ok {
		return a.token()
	}
	return ""
}

// Unexplained is the transport Explained was given, or t.
func Unexplained(t Transport) Transport {
	if e, ok := t.(explained); ok {
		return e.Transport
	}
	return t
}
