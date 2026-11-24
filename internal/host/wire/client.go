package wire

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/fault"
	"oikade.dev/internal/ident"
)

// requestTimeout bounds a request. A host that doesn't answer in this long is as good as gone.
const requestTimeout = 30 * time.Second

// Events are what a Client hears from the host. They arrive on its read goroutine and must not
// block. Ping must not block either: it hands the answering to a goroutine that may wait.
type Events struct {
	Changed    func(Message)
	Discovered func(Device)
	// Seen says a device reported, in Message's Device and Time.
	Seen func(Message)
	// Availability says a device went offline, or came back, in Message's Device and Online.
	Availability func(Message)
	Ping         func(id uint64)
	// Paused says a rule was paused, or resumed if paused is false.
	Paused func(rule string, paused bool)
}

// Client is your process's end of a session.
//
// It's also the engine's view of the host, as its Arbitration, HoldStates and Setter. Any failure
// to reach the host ends the session, since a process cut off from the host can't act. So those
// methods have nowhere to report one but Done.
type Client struct {
	conn *Conn
	next atomic.Uint64

	mu       sync.Mutex
	pending  map[uint64]chan Message
	holds    map[string]engine.HoldState
	averages map[string]engine.AverageState
	signals  map[string]engine.SignalState

	done chan struct{}
	once sync.Once
	err  error
}

var (
	_ engine.Arbitration = (*Client)(nil)
	_ engine.HoldStates  = (*Client)(nil)
)

// Dial opens a session as the automations process, and returns the Welcome. settings are every device
// setting the home declares, and info says what the rules are, if the caller knows.
func Dial(ctx context.Context, socket string, rules []string, settings []DeviceControl, info ...RuleInfo) (*Client, Message, error) {
	return DialHello(ctx, socket, Message{Rules: rules, Info: info, Control: settings})
}

// DialHello is Dial with the rest of a Hello, such as the bridge's name. Its Type, Version and Role
// are filled in.
func DialHello(ctx context.Context, socket string, hello Message) (*Client, Message, error) {
	hello.Type, hello.Version, hello.Role = TypeHello, Version, RoleAutomations
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, Message{}, fmt.Errorf("connecting to the host: %w", err)
	}
	conn := NewConn(raw)
	if err := conn.Write(hello); err != nil {
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
	if welcome.Type != TypeWelcome {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("%w: %s", ErrRejected, welcome.Error)
	}
	for range welcome.Runs {
		m, err := conn.Read()
		if err != nil {
			_ = conn.Close()
			return nil, Message{}, fmt.Errorf("reading the host's unfinished runs: %w", err)
		}
		if m.Type != TypeRun {
			_ = conn.Close()
			return nil, Message{}, fmt.Errorf("reading the host's unfinished runs: got %q", m.Type)
		}
		welcome.Unfinished = append(welcome.Unfinished, m.Unfinished...)
	}
	_ = conn.SetReadDeadline(time.Time{})
	c := &Client{conn: conn, pending: map[uint64]chan Message{}, holds: welcome.Holds, done: make(chan struct{})}
	c.averages = welcome.AverageStates
	if c.averages == nil {
		c.averages = map[string]engine.AverageState{}
	}
	c.signals = welcome.SignalStates
	if c.signals == nil {
		c.signals = map[string]engine.SignalState{}
	}
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
		case TypeSeen:
			if ev.Seen != nil {
				ev.Seen(m)
			}
		case TypeAvailability:
			if ev.Availability != nil && m.Online != nil {
				ev.Availability(m)
			}
		case TypePing:
			ev.Ping(m.ID)
		case TypePause, TypeResume:
			if ev.Paused != nil {
				ev.Paused(m.Rule, m.Type == TypePause)
			}
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

func (c *Client) Pong(id uint64) {
	if err := c.conn.Write(Message{Type: TypePong, ID: id}); err != nil {
		c.fail(err)
	}
}

// RuleRun tells the host how a rule's run ended.
func (c *Client) RuleRun(rule, result string, t time.Time) {
	if err := c.conn.Write(Message{Type: TypeRuleRun, Rule: rule, Result: result, Time: t}); err != nil {
		c.fail(err)
	}
}

// RoomReads tells the host the rooms a hold has read, so a move can warn when it empties one.
func (c *Client) RoomReads(rule string, reads []engine.RoomRead) {
	if err := c.conn.Write(Message{Type: TypeRoomReads, Rule: rule, Reads: reads}); err != nil {
		c.fail(err)
	}
}

// Stopped tells the host why a hold couldn't tell whether it should be active, or "" once it can.
func (c *Client) Stopped(rule, why string) {
	if err := c.conn.Write(Message{Type: TypeStopped, Rule: rule, Stopped: why}); err != nil {
		c.fail(err)
	}
}

// SignalWhy tells the host what a signal's conditions say now, and the devices it leaves out as dead.
func (c *Client) SignalWhy(name, why string, dead []string) {
	if err := c.conn.Write(Message{Type: TypeSignalWhy, Rule: name, Why: why, Dead: dead}); err != nil {
		c.fail(err)
	}
}

// WhyNot tells the host why a case is inactive, or "" once it isn't, or can't tell.
func (c *Client) WhyNot(rule, why string) {
	if err := c.conn.Write(Message{Type: TypeWhyNot, Rule: rule, WhyNot: why}); err != nil {
		c.fail(err)
	}
}

// PushMetrics gives the host the process's rule metrics, to serve with its own.
func (c *Client) PushMetrics(text []byte) {
	if err := c.conn.Write(Message{Type: TypeMetrics, Metrics: text}); err != nil {
		c.fail(err)
	}
}

// Done closes when the session has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err says why the session ended, once Done has closed.
func (c *Client) Err() error { return c.err }

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
func (c *Client) Claim(ctx context.Context, dev ident.ID, name string, claim control.Claim) bool {
	r, err := c.request(ctx, Message{Type: TypeClaim, Device: dev.Name, Capability: name, Claim: &claim})
	c.failUnsaved(err)
	return err != nil || r.Arbitrated
}

// failUnsaved ends the session when a request about a claim failed. The claim may not be on disk,
// and the next process works out from what is.
func (c *Client) failUnsaved(err error) {
	if err != nil {
		c.fail(err)
	}
}

// ReleaseClaim implements engine.Arbitration, handling failure as Claim does.
func (c *Client) ReleaseClaim(ctx context.Context, dev ident.ID, name, source string) bool {
	r, err := c.request(ctx, Message{Type: TypeRelease, Device: dev.Name, Capability: name, Source: source})
	c.failUnsaved(err)
	return err != nil || r.Arbitrated
}

// ReleaseClaims implements engine.Arbitration.
func (c *Client) ReleaseClaims(ctx context.Context, source string, below control.Level) error {
	_, err := c.request(ctx, Message{Type: TypeReleaseAll, Source: source, Below: below})
	c.failUnsaved(err)
	return err
}

// Set writes a capability through the host, for the rule ctx says it's for.
func (c *Client) Set(ctx context.Context, dev ident.ID, capability string, value any) error {
	_, err := c.request(ctx, Message{
		Type: TypeSet, Device: dev.Name, Capability: capability, Value: value, Source: driver.Writer(ctx),
	})
	return err
}

// Journal appends an entry to a run's journal, and returns once it's on the host's disk. After a
// failure the entry may or may not be there, so the session ends, and the next process replays what
// is.
func (c *Client) Journal(ctx context.Context, run string, data []byte) (uint64, error) {
	fault.Hit(fault.ClientJournalBefore)
	r, err := c.request(ctx, Message{Type: TypeJournal, Run: run, Data: data})
	if err != nil {
		c.fail(err)
		return 0, err
	}
	fault.Hit(fault.ClientJournalAfter)
	return r.Seq, nil
}

// Finish forgets a run's journal, and returns once that's on the host's disk. A failure leaves the
// run to be replayed, and finished, by the next process.
func (c *Client) Finish(ctx context.Context, run string) error {
	fault.Hit(fault.ClientJournalBefore)
	if _, err := c.request(ctx, Message{Type: TypeFinish, Run: run}); err != nil {
		return err
	}
	fault.Hit(fault.ClientJournalAfter)
	return nil
}

// SaveFire records when a timer was last accounted for, so one missed while no process ran can be
// caught up.
func (c *Client) SaveFire(ctx context.Context, rule string, t time.Time) error {
	_, err := c.request(ctx, Message{Type: TypeSaveFire, Rule: rule, Time: t})
	return err
}

// Get implements engine.HoldStates from what the Welcome returned and what's been saved since. The
// engine calls it with its lock held, so it never reaches the host.
func (c *Client) Get(key ident.ID) (engine.HoldState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.holds[key.Name]
	return s, ok
}

// Put implements engine.HoldStates. It returns once the state is on the host's disk.
func (c *Client) Put(ctx context.Context, key ident.ID, s engine.HoldState) error {
	if _, err := c.request(ctx, Message{Type: TypeSaveHold, Rule: key.Name, Hold: &s}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holds[key.Name] = s
	return nil
}

// Delete implements engine.HoldStates.
func (c *Client) Delete(ctx context.Context, key ident.ID) error {
	if _, err := c.request(ctx, Message{Type: TypeDeleteHold, Rule: key.Name}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.holds, key.Name)
	return nil
}

// Averages is the engine's AverageStates, kept on the host as holds are.
func (c *Client) Averages() engine.AverageStates { return clientAverages{c} }

type clientAverages struct{ c *Client }

// Get implements engine.AverageStates from what the Welcome returned and what's been saved since.
func (a clientAverages) Get(key ident.ID) (engine.AverageState, bool) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	s, ok := a.c.averages[key.Name]
	return s, ok
}

// Put implements engine.AverageStates. It returns once the state is on the host's disk.
func (a clientAverages) Put(ctx context.Context, key ident.ID, s engine.AverageState) error {
	if _, err := a.c.request(ctx, Message{Type: TypeSaveAverage, Rule: key.Name, Average: &s}); err != nil {
		return err
	}
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	a.c.averages[key.Name] = s
	return nil
}

// Delete implements engine.AverageStates. An average the home no longer makes is left on the host,
// where it's small, and kept for a version that makes it again.
func (a clientAverages) Delete(_ context.Context, key ident.ID) error {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	delete(a.c.averages, key.Name)
	return nil
}

// Signals is the engine's SignalStates, kept on the host as holds are.
func (c *Client) Signals() engine.SignalStates { return clientSignals{c} }

type clientSignals struct{ c *Client }

// Get implements engine.SignalStates from what the Welcome returned and what's been saved since.
func (s clientSignals) Get(key ident.ID) (engine.SignalState, bool) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	st, ok := s.c.signals[key.Name]
	return st, ok
}

// Put implements engine.SignalStates. It returns once the state is on the host's disk.
func (s clientSignals) Put(ctx context.Context, key ident.ID, st engine.SignalState) error {
	if _, err := s.c.request(ctx, Message{Type: TypeSaveSignal, Rule: key.Name, Signal: &st}); err != nil {
		return err
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.signals[key.Name] = st
	return nil
}

// Delete implements engine.SignalStates. A signal the home no longer makes is left on the host, as
// an average is.
func (s clientSignals) Delete(_ context.Context, key ident.ID) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	delete(s.c.signals, key.Name)
	return nil
}
