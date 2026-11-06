package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	// helloTimeout is how long a new connection has to say who it is.
	helloTimeout = 5 * time.Second
	// outgoing bounds what's waiting for a process. One that falls this far behind is cut off, and
	// starts again from a fresh Welcome.
	outgoing = 4096
	// requests bounds a session's requests in progress.
	requests = 256
)

// server serves the socket. It's a driver.Sink, streaming what the host hears to the session.
type server struct {
	hub.NopSink
	log     logr.Logger
	ctx     context.Context
	devices *hub.Store
	engine  *engine.Engine
	setter  driver.Setter
	holds   engine.HoldStates
	journal *store.Journal

	mu      sync.Mutex
	session *session
	// pong is when the session last answered a ping.
	pong time.Time
}

// session is the connected automations process.
type session struct {
	conn *wire.Conn
	out  chan wire.Message
	gone chan struct{}
	once sync.Once
}

func (s *session) end() {
	s.once.Do(func() {
		_ = s.conn.Close()
		close(s.gone)
	})
}

// serve accepts connections until the listener closes.
func (s *server) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Error(err, "Failed to accept connection")
			}
			return
		}
		go s.handle(wire.NewConn(c))
	}
}

// listen opens the socket, replacing one left by a host that died. The store's lock means no other
// host can be using it.
func listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Listen leaves the mode to the umask. Whoever reaches this socket runs the home.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func (s *server) handle(conn *wire.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, err := conn.Read()
	if err != nil {
		s.log.V(1).Info("Dropped connection before its hello", "error", err.Error())
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if reason := refusal(hello); reason != "" {
		s.log.Info("Refused connection", "reason", reason)
		_ = conn.Write(wire.Message{Type: wire.TypeRejected, Error: reason})
		return
	}
	sess := &session{conn: conn, out: make(chan wire.Message, outgoing), gone: make(chan struct{})}
	if !s.attach(sess, hello) {
		s.log.Info("Refused automations process; one is already connected")
		_ = conn.Write(wire.Message{Type: wire.TypeRejected, Error: "an automations process is already connected"})
		return
	}
	defer s.detach(sess)
	s.log.Info("Connected automations process", "rules", len(hello.Rules))

	go s.write(sess)
	work := make(chan wire.Message, requests)
	go func() {
		for m := range work {
			s.send(sess, s.answer(m))
		}
	}()
	defer close(work)
	for {
		m, err := conn.Read()
		if err != nil {
			select {
			case <-sess.gone:
			default:
				s.log.Info("Disconnected automations process", "reason", err.Error())
			}
			return
		}
		if m.Type == wire.TypePong {
			s.mu.Lock()
			s.pong = time.Now()
			s.mu.Unlock()
			continue
		}
		// Requests wait for the disk, which pongs mustn't, so they run apart. They run in order.
		select {
		case work <- m:
		case <-sess.gone:
			return
		}
	}
}

func refusal(hello wire.Message) string {
	switch {
	case hello.Type != wire.TypeHello:
		return fmt.Sprintf("expected a hello, got %q", hello.Type)
	case hello.Version != wire.Version:
		return fmt.Sprintf("protocol version %d isn't supported, want %d", hello.Version, wire.Version)
	case hello.Role != wire.RoleAutomations:
		return fmt.Sprintf("unknown role %q", hello.Role)
	}
	return ""
}

// attach makes sess the session and queues its Welcome, unless there is one already. The lock
// orders the Welcome before anything sent after it. A change that lands between the snapshot and
// the attach's end reaches the process twice at worst, and it ignores a value it already has.
func (s *server) attach(sess *session, hello wire.Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		return false
	}
	s.session, s.pong = sess, time.Now()
	welcome := wire.Message{Type: wire.TypeWelcome, Version: wire.Version, Holds: map[string]engine.HoldState{}}
	for key, snap := range s.devices.List() {
		if key.Namespace == wire.Namespace {
			welcome.Devices = append(welcome.Devices, deviceOf(key.Name, snap))
		}
	}
	for _, name := range hello.Rules {
		if st, ok := s.holds.Get(key(name)); ok {
			welcome.Holds[name] = st
		}
	}
	sess.out <- welcome
	return true
}

func (s *server) detach(sess *session) {
	sess.end()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == sess {
		s.session = nil
	}
}

// disconnect ends the session, if there is one, at once, so that the next process can't be refused
// over the old one's connection.
func (s *server) disconnect() {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.mu.Unlock()
	if sess != nil {
		sess.end()
	}
}

// write sends the session's messages in order until it ends.
func (s *server) write(sess *session) {
	for {
		select {
		case m := <-sess.out:
			if err := sess.conn.Write(m); err != nil {
				s.log.Info("Disconnected automations process", "reason", err.Error())
				sess.end()
				return
			}
		case <-sess.gone:
			return
		}
	}
}

// send queues a message without blocking the driver that caused it. A process too far behind is
// cut off.
func (s *server) send(sess *session, m wire.Message) {
	select {
	case sess.out <- m:
	case <-sess.gone:
	default:
		s.log.Info("Disconnected automations process; it fell behind")
		sess.end()
	}
}

// broadcast sends a message to the session, if there is one.
func (s *server) broadcast(m wire.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		s.send(s.session, m)
	}
}

// ping asks the session to answer, and returns when it last did. ok is false with no session.
func (s *server) ping(id uint64) (last time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return time.Time{}, false
	}
	s.send(s.session, wire.Message{Type: wire.TypePing, ID: id})
	return s.pong, true
}

// Changed implements driver.Sink.
func (s *server) Changed(u driver.Update) {
	if u.Device.Namespace != wire.Namespace {
		return
	}
	s.broadcast(wire.Message{
		Type: wire.TypeChange, Device: u.Device.Name, Capability: u.Capability,
		Value: u.Value, Previous: u.Previous, Time: u.Time,
	})
}

// Discovered implements driver.Sink.
func (s *server) Discovered(dev types.NamespacedName, _ driver.Info) {
	snap, ok := s.devices.Get(dev)
	if !ok || dev.Namespace != wire.Namespace {
		return
	}
	d := deviceOf(dev.Name, snap)
	s.broadcast(wire.Message{Type: wire.TypeDiscovered, Discovered: &d})
}

// answer carries out a request and returns its Reply.
func (s *server) answer(m wire.Message) wire.Message {
	reply := wire.Message{Type: wire.TypeReply, ID: m.ID}
	var err error
	switch m.Type {
	case wire.TypeClaim:
		if m.Claim == nil {
			err = errors.New("a claim needs one")
			break
		}
		reply.Arbitrated = s.engine.Claim(s.ctx, key(m.Device), m.Capability, *m.Claim)
	case wire.TypeRelease:
		reply.Arbitrated = s.engine.ReleaseClaim(s.ctx, key(m.Device), m.Capability, m.Source)
	case wire.TypeReleaseAll:
		s.engine.ReleaseClaims(s.ctx, wire.Namespace, m.Source)
	case wire.TypeSet:
		err = s.setter.Set(s.ctx, key(m.Device), m.Capability, m.Value)
	case wire.TypeSaveHold:
		if m.Hold == nil {
			err = errors.New("saving a hold needs its state")
			break
		}
		err = s.holds.Put(s.ctx, key(m.Rule), *m.Hold)
	case wire.TypeDeleteHold:
		err = s.holds.Delete(s.ctx, key(m.Rule))
	case wire.TypeJournal:
		reply.Seq, err = s.journal.Append(m.Run, m.Data)
	default:
		err = fmt.Errorf("unknown request %q", m.Type)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: wire.Namespace, Name: name}
}

func deviceOf(name string, snap hub.Snapshot) wire.Device {
	d := wire.Device{
		Name: name, Room: snap.Room, Discovered: snap.Discovered,
		Capabilities: slices.Clone(snap.Info.Capabilities), Values: snap.Values,
	}
	for _, c := range snap.Control {
		d.Arbitrated = append(d.Arbitrated, c.Capability)
	}
	return d
}
