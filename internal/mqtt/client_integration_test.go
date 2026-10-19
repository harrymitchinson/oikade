/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mqtt_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

func TestClientAgainstBroker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := mqtttest.StartBroker(t)

	pub, err := mqtt.Connect(ctx, url, "pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(ctx, "z2m/bridge/devices", []byte("[]"), true); err != nil {
		t.Fatal(err)
	}

	sub, err := mqtt.Connect(ctx, url, "sub")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan mqtt.Message, 10)
	if _, err := sub.Subscribe(ctx, "z2m/#", func(m mqtt.Message) { got <- m }); err != nil {
		t.Fatal(err)
	}

	expect := func(topic string, retained bool) {
		t.Helper()
		select {
		case m := <-got:
			if m.Topic != topic || m.Retained != retained {
				t.Fatalf("got %s (retained=%v), want %s (retained=%v)", m.Topic, m.Retained, topic, retained)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", topic)
		}
	}
	expect("z2m/bridge/devices", true)

	if err := pub.Publish(ctx, "z2m/lamp", []byte(`{"state":"ON"}`), false); err != nil {
		t.Fatal(err)
	}
	expect("z2m/lamp", false)

	// After a dropped connection the subscription comes back, retained messages included.
	mqtt.TerminateForTest(sub)
	expect("z2m/bridge/devices", true)
	if err := pub.Publish(ctx, "z2m/lamp", []byte(`{"state":"OFF"}`), false); err != nil {
		t.Fatal(err)
	}
	expect("z2m/lamp", false)
}

// receive subscribes a fresh client to filter and returns its messages.
func receive(ctx context.Context, t *testing.T, url, clientID, filter string) <-chan mqtt.Message {
	t.Helper()
	c, err := mqtt.Connect(ctx, url, clientID)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan mqtt.Message, 10)
	if _, err := c.Subscribe(ctx, filter, func(m mqtt.Message) { got <- m }); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWillOnDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := mqtttest.StartBroker(t)

	live := receive(ctx, t, url, "live", "p/#")
	plugin, err := mqtt.Connect(ctx, url, "plugin", mqtt.WithWill("p/status", []byte("offline")))
	if err != nil {
		t.Fatal(err)
	}
	mqtt.TerminateForTest(plugin)
	select {
	case m := <-live:
		if m.Topic != "p/status" || string(m.Payload) != "offline" {
			t.Fatalf("got %s=%q, want the Will", m.Topic, m.Payload)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the Will")
	}

	// A client subscribing afterwards gets the Will, so it was retained.
	select {
	case m := <-receive(ctx, t, url, "late", "p/#"):
		if m.Topic != "p/status" || string(m.Payload) != "offline" || !m.Retained {
			t.Fatalf("got %s=%q (retained=%v), want the retained Will", m.Topic, m.Payload, m.Retained)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the Will")
	}
}

func TestNoWillOnDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b := mqtttest.NewBroker(t)

	got := receive(ctx, t, b.URL, "watcher", "p/#")
	plugin, err := mqtt.Connect(ctx, b.URL, "plugin", mqtt.WithWill("p/status", []byte("offline")))
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	b.WaitGone(t, "plugin")

	// The watcher's next message is the sentinel, not a Will.
	pub, err := mqtt.Connect(ctx, b.URL, "pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(ctx, "p/sentinel", nil, false); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.Topic != "p/sentinel" {
			t.Fatalf("got %s=%q, want no Will", m.Topic, m.Payload)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the sentinel")
	}
}

func TestOnConnectAfterResubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := mqtttest.NewBroker(t)

	// OnConnect publishes to a topic the client subscribes to itself. It only hears its own
	// announcement if the subscription was restored before OnConnect ran.
	// The first OnConnect can run before Connect returns, so the client is handed over.
	clientCh := make(chan *mqtt.Client, 1)
	// OnConnect reports its publish result here rather than calling t from a client goroutine.
	connects := make(chan error, 10)
	client, err := mqtt.Connect(ctx, b.URL, "plugin", mqtt.WithOnConnect(func(ctx context.Context) {
		var c *mqtt.Client
		select {
		case c = <-clientCh:
			clientCh <- c
		case <-ctx.Done():
			return
		}
		connects <- c.Publish(ctx, "p/hello", nil, false)
	}))
	if err != nil {
		t.Fatal(err)
	}
	clientCh <- client
	defer func() { _ = client.Disconnect(context.Background()) }()
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	connected := func(what string) {
		t.Helper()
		select {
		case err := <-connects:
			if err != nil {
				t.Fatalf("publishing from %s: %v", what, err)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	connected("the first OnConnect")

	heard := make(chan struct{}, 10)
	if _, err := client.Subscribe(ctx, "p/hello", func(mqtt.Message) { heard <- struct{}{} }); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		b.Restart(t)
		connected(fmt.Sprint("OnConnect after restart ", i+1))
		wait(heard, fmt.Sprint("OnConnect's announcement after restart ", i+1))
	}
}
