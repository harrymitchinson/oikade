package mqtt_test

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"oikade.dev/internal/mqtt"
	"oikade.dev/internal/mqtt/mqtttest"
)

// TestClientFindsALocalBroker names the broker broker.local, and checks that the client dials it
// through the .local dialer rather than Go's resolver, which can't find it.
func TestClientFindsALocalBroker(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	u, err := url.Parse(mqtttest.StartBroker(t))
	if err != nil {
		t.Fatal(err)
	}
	real := u.Host
	u.Host = net.JoinHostPort("broker.local", u.Port())
	asked := make(chan string, 1)
	mqtt.SetLocalDialer(t, func(ctx context.Context, network, address string) (net.Conn, error) {
		select {
		case asked <- address:
		default:
		}
		return (&net.Dialer{}).DialContext(ctx, network, real)
	})

	c, err := mqtt.Connect(ctx, u.String(), "local")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(ctx) }()
	if got := <-asked; got != u.Host {
		t.Errorf("dialed %s, want %s", got, u.Host)
	}
	got := make(chan mqtt.Message, 1)
	if _, err := c.Subscribe(ctx, "lamp", func(m mqtt.Message) { got <- m }); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, "lamp", []byte("on"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if string(m.Payload) != "on" {
			t.Errorf("got %q, want on", m.Payload)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the message")
	}
}
