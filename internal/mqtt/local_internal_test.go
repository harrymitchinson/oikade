package mqtt

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"
)

func TestDialLocalRefusesWebSockets(t *testing.T) {
	dialed := false
	attempt := dialLocal(func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("dialed")
	})
	for _, scheme := range []string{"ws", "wss"} {
		_, err := attempt(t.Context(), autopaho.ClientConfig{}, &url.URL{Scheme: scheme, Host: "broker.local:9001"})
		if err == nil || !strings.Contains(err.Error(), "mqtt://") {
			t.Errorf("%s: got %v, want an error naming mqtt://", scheme, err)
		}
	}
	if dialed {
		t.Error("dialed a WebSocket broker")
	}
}
