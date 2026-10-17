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
	"testing"
	"time"

	"github.com/harry/oikade/internal/mqtt"
	"github.com/harry/oikade/internal/mqtt/mqtttest"
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
	if err := sub.Subscribe(ctx, "z2m/#", func(m mqtt.Message) { got <- m }); err != nil {
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
