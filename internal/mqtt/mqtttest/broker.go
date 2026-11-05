// Package mqtttest runs an in-process MQTT broker for tests.
package mqtttest

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
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
	b, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

// Listen starts a broker on a free localhost port, for callers outside a test such as the
// conformance command. Close stops it.
func Listen() (*Broker, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	_ = l.Close()

	b := &Broker{URL: "mqtt://" + addr, addr: addr}
	if err := b.start(); err != nil {
		return nil, err
	}
	return b, nil
}

// Close stops the broker.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	closed := make(chan struct{})
	go func() {
		closeServer(b.s)
		close(closed)
	}()
	// If mochi deadlocks anyway, leave it rather than hang.
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
	}
}

func (b *Broker) start() error {
	s := mochi.New(&mochi.Options{Logger: slog.New(slog.DiscardHandler)})
	if err := s.AddHook(new(auth.AllowHook), nil); err != nil {
		return err
	}
	if err := s.AddHook(&oneSession{s: s}, nil); err != nil {
		return err
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
		return err
	}
	go func() { _ = s.Serve() }()
	b.mu.Lock()
	b.s = s
	b.mu.Unlock()
	return nil
}

// Restart stops the broker, dropping every connection and retained message, and starts a new
// one on the same address.
func (b *Broker) Restart(t testing.TB) {
	t.Helper()
	b.mu.Lock()
	closeServer(b.s)
	b.mu.Unlock()
	if err := b.start(); err != nil {
		t.Fatal(err)
	}
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

// oneSession makes a reconnecting client wait until the broker has finished with its old
// connection. mochi v2.7.9 tears that down on its own goroutine, so a client that reconnects at
// once races it: the old Will can land after the new session announced itself, and the teardown
// can delete the new client from the broker's list, which leaves it connected and subscribed, but
// hearing nothing except retained messages. Mosquitto ends the old session first.
type oneSession struct {
	mochi.HookBase
	s *mochi.Server
}

// ID implements mochi.Hook.
func (h *oneSession) ID() string { return "one-session" }

// Provides implements mochi.Hook.
func (h *oneSession) Provides(b byte) bool { return b == mochi.OnConnect }

// OnConnect implements mochi.Hook. It ends clientID's old connection and waits for its teardown,
// Will included. These sessions expire on disconnect, so the client always subscribes again.
func (h *oneSession) OnConnect(cl *mochi.Client, _ packets.Packet) error {
	old, ok := h.s.Clients.Get(cl.ID)
	if !ok {
		return nil
	}
	old.Stop(errors.New("replaced by a new connection"))
	for range 5000 {
		if current, ok := h.s.Clients.Get(cl.ID); !ok || current != old {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("client %s: old connection still closing", cl.ID)
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
