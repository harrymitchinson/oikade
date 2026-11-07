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
