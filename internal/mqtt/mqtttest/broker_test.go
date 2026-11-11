package mqtttest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/paho"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// slowTeardown holds a closing connection at the points where it can race the client's reconnect.
type slowTeardown struct {
	mochi.HookBase
	will, unsubscribed time.Duration
}

func (h *slowTeardown) ID() string { return "slow-teardown" }

func (h *slowTeardown) Provides(b byte) bool { return b == mochi.OnWill || b == mochi.OnUnsubscribed }

func (h *slowTeardown) OnWill(_ *mochi.Client, w mochi.Will) (mochi.Will, error) {
	time.Sleep(h.will)
	return w, nil
}

func (h *slowTeardown) OnUnsubscribed(*mochi.Client, packets.Packet) { time.Sleep(h.unsubscribed) }

func TestReconnectWaitsForTheOldConnection(t *testing.T) {
	const slow = 200 * time.Millisecond
	tests := []struct {
		name string
		hook *slowTeardown
	}{
		{"its Will goes first", &slowTeardown{will: slow}},
		{"its teardown leaves the new client alone", &slowTeardown{unsubscribed: slow}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			b := NewBroker(t)
			if err := b.s.AddHook(tt.hook, nil); err != nil {
				t.Fatal(err)
			}
			other, err := mqtt.Connect(ctx, b.URL, "other")
			if err != nil {
				t.Fatal(err)
			}
			watched := make(chan mqtt.Message, 10)
			if _, err := other.Subscribe(ctx, "p/#", func(m mqtt.Message) { watched <- m }); err != nil {
				t.Fatal(err)
			}

			connected := make(chan struct{}, 10)
			client, err := mqtt.Connect(ctx, b.URL, "client", mqtt.WithWill("p/status", []byte("offline")),
				mqtt.WithOnConnect(func(context.Context) { connected <- struct{}{} }))
			if err != nil {
				t.Fatal(err)
			}
			heard := make(chan struct{}, 10)
			if _, err := client.Subscribe(ctx, "q/live", func(mqtt.Message) { heard <- struct{}{} }); err != nil {
				t.Fatal(err)
			}
			wait := func(ch <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-ch:
				case <-ctx.Done():
					t.Fatalf("timed out waiting for %s", what)
				}
			}
			wait(connected, "the first connection")

			b.Kick(t, "client")
			wait(connected, "the reconnect")
			if err := other.Publish(ctx, "p/sentinel", nil, false); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-watched:
				if m.Topic != "p/status" {
					t.Fatalf("got %s before the Will", m.Topic)
				}
			case <-ctx.Done():
				t.Fatal("timed out waiting for the Will")
			}

			// Past the old connection's teardown, the new client still hears live messages.
			time.Sleep(2 * tt.hook.unsubscribed)
			if err := other.Publish(ctx, "q/live", nil, false); err != nil {
				t.Fatal(err)
			}
			wait(heard, "a live message")
		})
	}
}

// A session that outlives its connection stays in the broker's list, so waiting for it to leave
// would hold the reconnect up.
func TestReconnectTakesOverALastingSession(t *testing.T) {
	b := NewBroker(t)
	expiry := uint32(60)
	connect := func() *paho.Client {
		t.Helper()
		// Well inside oneSession's 5s.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := net.Dial("tcp", strings.TrimPrefix(b.URL, "mqtt://"))
		if err != nil {
			t.Fatal(err)
		}
		c := paho.NewClient(paho.ClientConfig{Conn: conn})
		if _, err := c.Connect(ctx, &paho.Connect{
			ClientID: "lasting", KeepAlive: 30, Properties: &paho.ConnectProperties{SessionExpiryInterval: &expiry},
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	_ = connect().Disconnect(&paho.Disconnect{ReasonCode: 0})
	connected := connect()
	t.Cleanup(func() { _ = connected.Disconnect(&paho.Disconnect{ReasonCode: 0}) })
}

// Under -race, a wildcard subscription's retained scan while a retained message is stored, by a
// PUBLISH or by a Will, is a data race inside mochi v2.7.9 unless retainScan keeps them apart.
func TestRetainedStoreAndScanDontRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b := NewBroker(t)
	publisher, err := mqtt.Connect(ctx, b.URL, "publisher")
	if err != nil {
		t.Fatal(err)
	}
	// At QoS 0, so that the retained messages it is sent take no packet IDs: mochi refuses a
	// SUBSCRIBE whose ID one of them holds.
	conn, err := net.Dial("tcp", strings.TrimPrefix(b.URL, "mqtt://"))
	if err != nil {
		t.Fatal(err)
	}
	subscriber := paho.NewClient(paho.ClientConfig{Conn: conn})
	if _, err := subscriber.Connect(ctx, &paho.Connect{ClientID: "subscriber", KeepAlive: 30, CleanStart: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscriber.Disconnect(&paho.Disconnect{ReasonCode: 0}) })
	connected := make(chan struct{}, 100)
	if _, err := mqtt.Connect(ctx, b.URL, "dropped", mqtt.WithWill("r/will", []byte("offline")),
		mqtt.WithOnConnect(func(context.Context) { connected <- struct{}{} })); err != nil {
		t.Fatal(err)
	}
	<-connected

	const rounds = 30
	stored := make(chan error, 1)
	go func() {
		for i := range rounds {
			if err := publisher.Publish(ctx, "r/published", []byte{byte(i)}, true); err != nil {
				stored <- err
				return
			}
			if i%10 == 0 {
				b.Kick(t, "dropped")
				select {
				case <-connected:
				case <-ctx.Done():
					stored <- ctx.Err()
					return
				}
			}
		}
		stored <- nil
	}()
	for range rounds {
		if _, err := subscriber.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: "r/+"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-stored; err != nil {
		t.Fatal(err)
	}
}
