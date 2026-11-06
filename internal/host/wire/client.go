package wire

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
)

// requestTimeout bounds a request. A host that doesn't answer in this long is as good as gone.
const requestTimeout = 30 * time.Second

// Events are what a Client hears from the host. They arrive on its read goroutine and must not
// block. Ping must not block either: it hands the answering to a goroutine that may wait.
type Events struct {
	Changed    func(Message)
	Discovered func(Device)
	Ping       func(id uint64)
}

// Client is your process's end of a session.
//
// It's also the engine's view of the host, as its Arbitration, HoldStates and Setter. Any failure
// to reach the host ends the session, since a process cut off from the host can't act. So those
// methods have nowhere to report one but Done.
type Client struct {
	conn *Conn
	next atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan Message
	holds   map[string]engine.HoldState

	done chan struct{}
	once sync.Once
	err  error
}

var (
	_ engine.Arbitration = (*Client)(nil)
	_ engine.HoldStates  = (*Client)(nil)
)

// Dial opens a session as the automations process, and returns the Welcome.
func Dial(ctx context.Context, socket string, rules []string) (*Client, Message, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, Message{}, fmt.Errorf("connecting to the host: %w", err)
	}
	conn := NewConn(raw)
	if err := conn.Write(Message{Type: TypeHello, Version: Version, Role: RoleAutomations, Rules: rules}); err != nil {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("greeting the host: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	welcome, err := conn.Read()
	if err != nil {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("reading the host's welcome: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if welcome.Type != TypeWelcome {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("%w: %s", ErrRejected, welcome.Error)
	}
	c := &Client{conn: conn, pending: map[uint64]chan Message{}, holds: welcome.Holds, done: make(chan struct{})}
	if c.holds == nil {
		c.holds = map[string]engine.HoldState{}
	}
	return c, welcome, nil
}

// Listen reads from the host until the session ends. Run it once, after setting up what the events
// reach: the host streams from its Welcome on.
func (c *Client) Listen(ev Events) {
	for {
		m, err := c.conn.Read()
		if err != nil {
			c.fail(err)
			return
		}
		switch m.Type {
		case TypeChange:
			ev.Changed(m)
		case TypeDiscovered:
			if m.Discovered != nil {
				ev.Discovered(*m.Discovered)
			}
		case TypePing:
			ev.Ping(m.ID)
		case TypeReply:
			c.mu.Lock()
			reply := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if reply != nil {
				reply <- m
			}
		}
	}
}

// Pong answers a ping.
func (c *Client) Pong(id uint64) {
	if err := c.conn.Write(Message{Type: TypePong, ID: id}); err != nil {
		c.fail(err)
	}
}

// Done closes when the session has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err says why the session ended, once Done has closed.
func (c *Client) Err() error { return c.err }

// Close ends the session.
func (c *Client) Close() { c.fail(nil) }

func (c *Client) fail(err error) {
	c.once.Do(func() {
		c.err = err
		_ = c.conn.Close()
		close(c.done)
	})
}

// request sends m and waits for its Reply, however ctx ends. A request is the host's record of what
// the process did, so one under way when a run is cancelled still needs its answer.
func (c *Client) request(ctx context.Context, m Message) (Message, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	m.ID = c.next.Add(1)
	reply := make(chan Message, 1)
	c.mu.Lock()
	c.pending[m.ID] = reply
	c.mu.Unlock()
	if err := c.conn.Write(m); err != nil {
		c.fail(err)
		return Message{}, err
	}
	select {
	case r := <-reply:
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		err := fmt.Errorf("the host didn't answer a %s", m.Type)
		c.fail(err)
		return Message{}, err
	case <-c.done:
		return Message{}, errors.New("the session has ended")
	}
}

// Claim implements engine.Arbitration. If the host can't be reached the session is ending, and the
// next process claims again, so it reports the claim as handled rather than write around it.
func (c *Client) Claim(ctx context.Context, dev types.NamespacedName, name string, claim control.Claim) bool {
	r, err := c.request(ctx, Message{Type: TypeClaim, Device: dev.Name, Capability: name, Claim: &claim})
	return err != nil || r.Arbitrated
}

// ReleaseClaim implements engine.Arbitration, handling failure as Claim does.
func (c *Client) ReleaseClaim(ctx context.Context, dev types.NamespacedName, name, source string) bool {
	r, err := c.request(ctx, Message{Type: TypeRelease, Device: dev.Name, Capability: name, Source: source})
	return err != nil || r.Arbitrated
}

// ReleaseClaims implements engine.Arbitration.
func (c *Client) ReleaseClaims(ctx context.Context, _, source string) {
	_, _ = c.request(ctx, Message{Type: TypeReleaseAll, Source: source})
}

// Set writes a capability through the host.
func (c *Client) Set(ctx context.Context, dev types.NamespacedName, capability string, value any) error {
	_, err := c.request(ctx, Message{Type: TypeSet, Device: dev.Name, Capability: capability, Value: value})
	return err
}

// Journal appends an entry to a run's journal, and returns once it's on the host's disk.
func (c *Client) Journal(ctx context.Context, run string, data []byte) (uint64, error) {
	r, err := c.request(ctx, Message{Type: TypeJournal, Run: run, Data: data})
	return r.Seq, err
}

// Get implements engine.HoldStates from what the Welcome returned and what's been saved since. The
// engine calls it with its lock held, so it never reaches the host.
func (c *Client) Get(key types.NamespacedName) (engine.HoldState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.holds[key.Name]
	return s, ok
}

// Put implements engine.HoldStates. It returns once the state is on the host's disk.
func (c *Client) Put(ctx context.Context, key types.NamespacedName, s engine.HoldState) error {
	if _, err := c.request(ctx, Message{Type: TypeSaveHold, Rule: key.Name, Hold: &s}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holds[key.Name] = s
	return nil
}

// Delete implements engine.HoldStates.
func (c *Client) Delete(ctx context.Context, key types.NamespacedName) error {
	if _, err := c.request(ctx, Message{Type: TypeDeleteHold, Rule: key.Name}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.holds, key.Name)
	return nil
}
