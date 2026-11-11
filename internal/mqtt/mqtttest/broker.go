// Package mqtttest runs an in-process MQTT broker for tests.
package mqtttest

import (
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
	one  *oneSession
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
	closeServer(b.s)
}

func (b *Broker) start() error {
	s := mochi.New(&mochi.Options{Logger: slog.New(slog.DiscardHandler)})
	if err := s.AddHook(new(auth.AllowHook), nil); err != nil {
		return err
	}
	one := &oneSession{s: s}
	if err := s.AddHook(one, nil); err != nil {
		return err
	}
	// Added last, so no hook after it can reject a packet it has locked for.
	if err := s.AddHook(&retainScan{s: s}, nil); err != nil {
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
	b.s, b.one = s, one
	b.mu.Unlock()
	return nil
}

// Start brings back a broker that was closed, on the same address.
func (b *Broker) Start(t testing.TB) {
	t.Helper()
	if err := b.start(); err != nil {
		t.Fatal(err)
	}
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
// window. A client that connects during Close can also leave it waiting forever, so it gets 5s.
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
	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
	}
}

// oneSession makes a reconnecting client wait until the broker has finished with its old
// connection. mochi v2.7.9 tears that down on its own goroutine, so a client that reconnects at
// once races it: the old Will can land after the new session announced itself, and the teardown
// can delete the new client from the broker's list, which leaves it connected and subscribed, but
// hearing nothing except retained messages.
type oneSession struct {
	mochi.HookBase
	s *mochi.Server
	// kicked holds the connections Kick closed, whose teardown and Will are already on their way.
	// It is keyed by connection because mochi reuses its clients.
	kicked sync.Map
}

// ID implements mochi.Hook.
func (h *oneSession) ID() string { return "one-session" }

// Provides implements mochi.Hook.
func (h *oneSession) Provides(b byte) bool {
	return b == mochi.OnSessionEstablish || b == mochi.OnDisconnect
}

// OnDisconnect implements mochi.Hook. mochi calls it after the Will and Stop, so a kicked
// connection needs no more waiting for.
func (h *oneSession) OnDisconnect(cl *mochi.Client, _ error, _ bool) {
	h.kicked.Delete(cl.Net.Conn)
}

// OnSessionEstablish implements mochi.Hook. It ends the connection holding clientID and waits for
// its teardown, Will included. A connection Kick closed is already ending, so it only waits for
// that: until the session is gone, or, for one that outlives its connection, until mochi has
// stopped it, which comes after the Will. Any other session that outlives its connection has no
// teardown to race, and is left to mochi's takeover, as is one still here after 5s.
func (h *oneSession) OnSessionEstablish(cl *mochi.Client, _ packets.Packet) {
	for range 5000 {
		old, ok := h.s.Clients.Get(cl.ID)
		if !ok {
			return
		}
		_, kicked := h.kicked.Load(old.Net.Conn)
		switch {
		case kicked && !expiresOnDisconnect(old) && old.Closed():
			return
		case kicked:
		case !expiresOnDisconnect(old):
			return
		case !old.Closed():
			_ = h.s.DisconnectClient(old, packets.ErrSessionTakenOver)
		}
		time.Sleep(time.Millisecond)
	}
}

// retainScan keeps a retained message from being stored while a subscription scans for them.
// mochi v2.7.9's TopicsIndex.RetainMessage writes a topic's retainPath under the topic's lock, but
// scanMessages reads it for a wildcard filter without one: a data race, fixed on mochi's
// unreleased release/2.8.0 (OIK-232). So a SUBSCRIBE, whose retained scan runs before
// OnPacketProcessed, holds mu for reading, and a retained PUBLISH or an immediate retained Will,
// which store it, hold it for writing.
//
// It leaves out what the tests don't do: a delayed Will, the inline client and $SYS, which mochi
// stores without a client's packet (a wildcard at the root skips $SYS). A hook added after it
// mustn't reject a packet, which would leave mu locked, and holds mu while it runs.
type retainScan struct {
	mochi.HookBase
	s  *mochi.Server
	mu sync.RWMutex
	// held maps a connection to the unlock for the packet it is processing. mochi reads and
	// processes a connection's packets on one goroutine, so it has at most one. It is keyed by
	// connection, as oneSession.kicked is, because mochi reuses its clients.
	held sync.Map
}

// ID implements mochi.Hook.
func (h *retainScan) ID() string { return "retain-scan" }

// Provides implements mochi.Hook.
func (h *retainScan) Provides(b byte) bool {
	return b == mochi.OnPacketRead || b == mochi.OnPacketProcessed || b == mochi.OnWill || b == mochi.OnWillSent
}

// OnPacketRead implements mochi.Hook. It locks for a packet that mochi will process and hand to
// OnPacketProcessed: one that fails validation returns before that, so it is left alone.
func (h *retainScan) OnPacketRead(cl *mochi.Client, pk packets.Packet) (packets.Packet, error) {
	switch {
	case pk.FixedHeader.Type == packets.Publish && pk.FixedHeader.Retain &&
		pk.PublishValidate(h.s.Options.Capabilities.TopicAliasMaximum) == packets.CodeSuccess:
		h.mu.Lock()
		h.held.Store(cl.Net.Conn, h.mu.Unlock)
	case pk.FixedHeader.Type == packets.Subscribe && pk.SubscribeValidate() == packets.CodeSuccess:
		h.mu.RLock()
		h.held.Store(cl.Net.Conn, h.mu.RUnlock)
	}
	return pk, nil
}

// OnPacketProcessed implements mochi.Hook.
func (h *retainScan) OnPacketProcessed(cl *mochi.Client, _ packets.Packet, _ error) {
	if unlock, ok := h.held.LoadAndDelete(cl.Net.Conn); ok {
		unlock.(func())()
	}
}

// OnWill implements mochi.Hook. mochi stores a retained Will between OnWill and OnWillSent,
// unless it delays the Will, when OnWillSent comes from another path.
func (h *retainScan) OnWill(_ *mochi.Client, w mochi.Will) (mochi.Will, error) {
	if w.Retain && w.WillDelayInterval == 0 {
		h.mu.Lock()
	}
	return w, nil
}

// OnWillSent implements mochi.Hook.
func (h *retainScan) OnWillSent(_ *mochi.Client, pk packets.Packet) {
	if pk.FixedHeader.Retain && pk.Connect.WillProperties.WillDelayInterval == 0 {
		h.mu.Unlock()
	}
}

// expiresOnDisconnect reports whether mochi removes cl from its list when the connection ends.
func expiresOnDisconnect(cl *mochi.Client) bool {
	if cl.Properties.ProtocolVersion == 5 {
		return cl.Properties.Props.SessionExpiryInterval == 0
	}
	return cl.Properties.Clean
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
// It only closes the socket: mochi's Stop also ends the read loop, which mochi takes for a clean
// disconnect and drops the Will, and so does the takeover of a reconnect that arrives first.
func (b *Broker) Kick(t testing.TB, clientID string) {
	t.Helper()
	b.mu.Lock()
	cl, ok := b.s.Clients.Get(clientID)
	one := b.one
	b.mu.Unlock()
	if !ok {
		t.Fatalf("client %s is not connected", clientID)
	}
	if _, again := one.kicked.LoadOrStore(cl.Net.Conn, struct{}{}); again || cl.Closed() {
		t.Fatalf("client %s is already disconnecting, so no new Will is coming", clientID)
	}
	_ = cl.Net.Conn.Close()
}
