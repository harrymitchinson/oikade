package mqtttest

import (
	"log/slog"
	"net"
	"sync"

	"github.com/mochi-mqtt/server/v2/listeners"
)

// listener is mochi's TCP listener, except that Close stops accepting before it drops the clients.
// mochi v2.7.9 drops them first, so a client that reconnects at once, as autopaho's first retry
// does, can be accepted in between and never served: it waits out its connect timeout, then its
// backoff, against a broker that has gone. Close also closes any connection accepted but not yet
// a client, which mochi's close of its clients can't see.
type listener struct {
	id   string
	addr string
	log  *slog.Logger

	mu       sync.Mutex
	l        net.Listener
	accepted map[net.Conn]bool
	closed   bool
}

var _ listeners.Listener = (*listener)(nil)

func newListener(id, addr string) *listener {
	return &listener{id: id, addr: addr, accepted: map[net.Conn]bool{}}
}

// boundListener serves a socket that's already bound.
func boundListener(id string, ln net.Listener) *listener {
	l := newListener(id, ln.Addr().String())
	l.l = ln
	return l
}

// Init implements listeners.Listener.
func (l *listener) Init(log *slog.Logger) error {
	l.log = log
	if l.l != nil {
		return nil
	}
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.l = ln
	l.mu.Unlock()
	return nil
}

// Serve implements listeners.Listener.
func (l *listener) Serve(establish listeners.EstablishFn) {
	for {
		conn, err := l.l.Accept()
		if err != nil {
			return
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return
		}
		l.accepted[conn] = true
		l.mu.Unlock()
		go func() {
			if err := establish(l.id, conn); err != nil {
				l.log.Debug("Ended connection", "err", err)
			}
			l.mu.Lock()
			delete(l.accepted, conn)
			l.mu.Unlock()
		}()
	}
}

// ID implements listeners.Listener.
func (l *listener) ID() string { return l.id }

// Address implements listeners.Listener.
func (l *listener) Address() string { return l.addr }

// Protocol implements listeners.Listener.
func (l *listener) Protocol() string { return "tcp" }

// Close implements listeners.Listener.
func (l *listener) Close(closeClients listeners.CloseFn) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	if l.l != nil {
		_ = l.l.Close()
	}
	l.mu.Unlock()
	closeClients(l.id)
	l.mu.Lock()
	defer l.mu.Unlock()
	for conn := range l.accepted {
		_ = conn.Close()
	}
}
