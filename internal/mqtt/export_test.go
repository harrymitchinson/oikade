package mqtt

import (
	"context"
	"net"
)

// TerminateForTest drops the connection so tests can exercise reconnects.
func TerminateForTest(c *Client) { c.cm.TerminateConnectionForTest() }

// SetLocalDialer replaces how a .local broker is dialed, until t ends.
func SetLocalDialer(t interface{ Cleanup(func()) }, dial func(ctx context.Context, network, address string) (net.Conn, error)) {
	old := localDialer
	localDialer = dial
	t.Cleanup(func() { localDialer = old })
}
