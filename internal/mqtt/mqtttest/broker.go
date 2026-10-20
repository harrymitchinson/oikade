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
		closed := make(chan struct{})
		go func() {
			closeServer(b.s)
			close(closed)
		}()
		// If mochi deadlocks anyway, leave it rather than hang the test binary.
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
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
	closeServer(b.s)
	b.mu.Unlock()
	b.start(t)
}

// closeServer closes s once no client is mid-disconnect. mochi v2.7.9's Close takes the clients'
// read lock twice (GetByListener calls Len), so a client being removed at the same moment, which
// waits for the write lock, deadlocks it. Waiting until the client count holds still closes that
// window.
func closeServer(s *mochi.Server) {
	last := s.Clients.Len()
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		n := s.Clients.Len()
		if n == last {
			break
		}
		last = n
	}
	_ = s.Close()
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
