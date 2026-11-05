package mqtttest

import (
	"context"
	"testing"
	"time"

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
			time.Sleep(2 * slow)
			if err := other.Publish(ctx, "q/live", nil, false); err != nil {
				t.Fatal(err)
			}
			wait(heard, "a live message")
		})
	}
}
