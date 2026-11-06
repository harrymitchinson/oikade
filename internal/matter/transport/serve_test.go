package transport

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

// failingConn fails every read, as a socket that has broken does.
type failingConn struct {
	net.PacketConn
	closed atomic.Bool
}

func (c *failingConn) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, errors.New("read failed")
}

func (c *failingConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *failingConn) LocalAddr() net.Addr { return &net.UDPAddr{} }

// A retry has to be able to bind the port again, so a failed Serve can't keep it.
func TestServeClosesTheSocketWhenItFails(t *testing.T) {
	conn := &failingConn{}
	if err := New(conn, Options{}).Serve(context.Background()); err == nil {
		t.Fatal("got no error from a failed read")
	}
	if !conn.closed.Load() {
		t.Fatal("the socket stayed open after Serve returned")
	}
}
