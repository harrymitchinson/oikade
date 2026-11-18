package api

import (
	"context"
	"errors"
	"net"
	"testing"
)

// A command's words for a host it can't reach change only what Dial says: the token still goes.
func TestExplainedKeepsTheToken(t *testing.T) {
	refused := errors.New("refused")
	explained := Explained(failingTransport{refused}, func(err error) error { return errors.Join(errors.New("explained"), err) })
	if _, err := explained.Dial(t.Context()); !errors.Is(err, refused) || err.Error() != "explained\nrefused" {
		t.Errorf("Dial: %v", err)
	}
	remote := Explained(Remote{Address: "pi.home:8443", Token: "oik_x"}, func(err error) error { return err })
	if a, ok := remote.(authorizer); !ok || a.token() != "oik_x" {
		t.Error("the token didn't go")
	}
	if Unexplained(remote) != (Remote{Address: "pi.home:8443", Token: "oik_x"}) {
		t.Errorf("Unexplained = %#v", Unexplained(remote))
	}
}

type failingTransport struct{ err error }

func (f failingTransport) Dial(context.Context) (net.Conn, error) { return nil, f.err }
