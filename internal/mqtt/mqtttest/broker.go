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

// Package mqtttest runs an in-process MQTT broker for tests.
package mqtttest

import (
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// Broker is an in-process broker on a fixed localhost port.
type Broker struct {
	// URL is the broker's mqtt:// URL. It stays the same across restarts.
	URL string

	addr string
	mu   sync.Mutex
	s    *mochi.Server
}

// StartBroker starts a broker on a free localhost port and returns its mqtt:// URL.
// It is stopped when the test ends.
func StartBroker(t testing.TB) string {
	t.Helper()
	return NewBroker(t).URL
}

// NewBroker starts a broker on a free localhost port. It is stopped when the test ends.
func NewBroker(t testing.TB) *Broker {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	b := &Broker{URL: "mqtt://" + addr, addr: addr}
	b.start(t)
	t.Cleanup(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		_ = b.s.Close()
	})
	return b
}

func (b *Broker) start(t testing.TB) {
	t.Helper()
	s := mochi.New(&mochi.Options{Logger: slog.New(slog.DiscardHandler)})
	if err := s.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	// After a Restart the old listener's port can take a moment to free up.
	var err error
	for range 50 {
		if err = s.AddListener(listeners.NewTCP(listeners.Config{ID: "test", Address: b.addr})); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	b.mu.Lock()
	b.s = s
	b.mu.Unlock()
}

// Restart stops the broker, dropping every connection and retained message, and starts a new
// one on the same address.
func (b *Broker) Restart(t testing.TB) {
	t.Helper()
	b.mu.Lock()
	_ = b.s.Close()
	b.mu.Unlock()
	b.start(t)
}

// WaitGone waits until the broker has finished with clientID's connection, including any Will.
func (b *Broker) WaitGone(t testing.TB, clientID string) {
	t.Helper()
	for range 200 {
		b.mu.Lock()
		_, ok := b.s.Clients.Get(clientID)
		b.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client %s still connected", clientID)
}

// Kick drops clientID's connection as a network failure would, so the broker publishes its Will.
func (b *Broker) Kick(t testing.TB, clientID string) {
	t.Helper()
	b.mu.Lock()
	cl, ok := b.s.Clients.Get(clientID)
	b.mu.Unlock()
	if !ok {
		t.Fatalf("client %s is not connected", clientID)
	}
	cl.Stop(errors.New("kicked by test"))
}
