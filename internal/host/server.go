package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
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
	log     *slog.Logger
	ctx     context.Context
	devices *hub.Store
	engine  *engine.Engine
	// setter is for your automations, and outside for a person's writes, which the log shows as the
	// change they make.
	setter  driver.Setter
	outside driver.Setter
	holds   *engine.Stored[engine.HoldState]
	fires   *engine.Stored[time.Time]
	journal *store.Journal
	// paused are the rules paused, with when.
	paused *engine.Stored[time.Time]
	// feed is what oikade tail streams, events what oikade replay reads, and pushed your process's
	// latest metrics.
	feed   *feed
	events *eventLog
	pushed *pushed

	mu      sync.Mutex
	session *session
	// closed refuses new sessions, once the host is stopping.
	closed bool
	// pong is when the session last answered a ping.
	pong time.Time
	// generation counts disconnects.
	generation uint64

	// connected, if set, hears of each session. It must not block.
	connected func()
	// plugins takes the connections of plugins.
	plugins *plugins
	// settings, if set, takes up what a connecting process declares, and confirmedSettings replaces
	// them with what a version that has settled declares.
	settings, confirmedSettings func([]wire.DeviceControl) error
	// attaching is held from a process's settings until it has attached: see open.
	attaching sync.Mutex
}

// session is the connected automations process.
type session struct {
	conn *wire.Conn
	out  chan wire.Message
	// gone closes when the connection does, and finished once its last request has been carried
	// out, so nothing from it can land on top of the next session.
	gone, finished chan struct{}
	once           sync.Once
	// generation is the server's when the connection arrived.
	generation uint64
	// rules are what the process's hello said about its rules.
	rules []wire.RuleInfo
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
				s.log.Error("Failed to accept connection", "err", err)
			}
			return
		}
		s.mu.Lock()
		generation := s.generation
		s.mu.Unlock()
		go s.handle(wire.NewConn(c), generation)
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

func (s *server) handle(conn *wire.Conn, generation uint64) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, err := conn.Read()
	if err != nil {
		s.log.Debug("Dropped connection before its hello", "err", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if reason := refusal(hello); reason != "" {
		s.log.Info("Refused connection", "reason", reason)
		_ = conn.Write(wire.Message{Type: wire.TypeRejected, Error: reason})
		return
	}
	if hello.Role == wire.RolePlugin {
		s.plugins.serve(conn, hello)
		return
	}
	sess := &session{
		conn: conn, out: make(chan wire.Message, outgoing), gone: make(chan struct{}), finished: make(chan struct{}),
		generation: generation, rules: hello.Info,
	}
	if refusal := s.open(sess, hello); refusal != "" {
		_ = conn.Write(wire.Message{Type: wire.TypeRejected, Error: refusal})
		return
	}
	s.log.Info("Connected automations process", "rules", len(hello.Rules))
	s.emit(api.Event{Time: time.Now(), Type: api.EventProcess, Message: "connected"})
	if s.connected != nil {
		s.connected()
	}

	work := make(chan wire.Message, requests)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for m := range work {
			s.send(sess, s.answer(m))
			if m.Type == wire.TypeClaim {
				fault.Hit(fault.HostClaimReplied)
			}
		}
	}()
	defer func() {
		close(work)
		<-drained
		s.detach(sess)
	}()
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
		switch m.Type {
		case wire.TypePong:
			s.mu.Lock()
			s.pong = time.Now()
			s.mu.Unlock()
			continue
		case wire.TypeRuleRun:
			s.emit(api.Event{Time: m.Time, Type: api.EventRule, Rule: m.Rule, Result: m.Result})
			continue
		case wire.TypeMetrics:
			s.pushed.set(m.Metrics)
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
	case hello.Role != wire.RoleAutomations && hello.Role != wire.RolePlugin:
		return fmt.Sprintf("unknown role %q", hello.Role)
	}
	return ""
}

// welcome reads a process's saved state: what its rules last saved, and the runs that didn't finish.
// It happens before attach takes the lock, which drivers wait on. No session is running to change
// any of it: a process refused by attach is told so, and the last one has finished.
func (s *server) welcome(hello wire.Message) (wire.Message, []wire.Run, error) {
	welcome := wire.Message{
		Type: wire.TypeWelcome, Version: wire.Version,
		Holds: map[string]engine.HoldState{}, Fires: map[string]time.Time{},
	}
	for _, name := range hello.Rules {
		if st, ok := s.holds.Get(key(name)); ok {
			welcome.Holds[name] = st
		}
		if t, ok := s.fires.Get(key(name)); ok {
			welcome.Fires[name] = t
		}
	}
	ids, err := s.journal.Runs()
	if err != nil {
		return wire.Message{}, nil, err
	}
	if welcome.Paused, err = s.paused.Names(wire.Namespace); err != nil {
		return wire.Message{}, nil, err
	}
	runs := make([]wire.Run, 0, len(ids))
	for _, id := range ids {
		// A pause ends a flow's run. A process that was running ended it, and if none was, this does.
		if slices.Contains(welcome.Paused, flow.FlowOf(id)) {
			if err := s.journal.Delete(id); err != nil {
				return wire.Message{}, nil, err
			}
			s.log.Info("Dropped flow run; its flow is paused", "run", id)
			continue
		}
		entries, err := s.journal.Entries(id)
		if err != nil {
			return wire.Message{}, nil, err
		}
		run := wire.Run{ID: id}
		for _, e := range entries {
			run.Entries = append(run.Entries, e.Data)
		}
		runs = append(runs, run)
	}
	welcome.Runs = len(runs)
	return welcome, runs, nil
}

// open takes up a process's settings and attaches it, or says why it can't. The two go together, so
// that a process turned away never had a say.
func (s *server) open(sess *session, hello wire.Message) string {
	s.attaching.Lock()
	defer s.attaching.Unlock()
	// Before the Welcome, so that it says what's arbitrated.
	if err := s.takeSettings(sess.generation, hello.Control); err != nil {
		s.log.Error("Failed to save the home's device settings", "err", err)
		return "the host couldn't save the home's device settings"
	}
	welcome, runs, err := s.welcome(hello)
	if err != nil {
		s.log.Error("Failed to read saved state for a process", "err", err)
		return "the host couldn't read its saved state"
	}
	if !s.attach(sess, welcome, runs) {
		s.log.Info("Refused automations process; one is already connected")
		return "an automations process is already connected"
	}
	return ""
}

// takeSettings takes up a process's device settings, unless attach is going to turn it away. Call
// with attaching held.
func (s *server) takeSettings(generation uint64, settings []wire.DeviceControl) error {
	s.mu.Lock()
	refused := s.session != nil || s.closed || generation != s.generation
	s.mu.Unlock()
	if refused || s.settings == nil {
		return nil
	}
	return s.settings(settings)
}

// confirmSettings makes the device settings exactly what a settled version declares.
func (s *server) confirmSettings(settings []wire.DeviceControl) error {
	if s.confirmedSettings == nil {
		return nil
	}
	return s.confirmedSettings(settings)
}

// safetyHeld says a Safety claim holds a capability.
func (s *server) safetyHeld(device, name string) bool {
	return slices.ContainsFunc(s.engine.ControlOf(key(device)), func(c control.Status) bool {
		return c.Capability == name && slices.ContainsFunc(c.Claims, func(o control.ClaimStatus) bool {
			return o.Level == control.NameSafety
		})
	})
}

// attach makes sess the session and queues its Welcome, unless there is one already. The lock
// orders the Welcome before anything sent after it. A change that lands between the snapshot and
// the attach's end reaches the process twice at worst, and it ignores a value it already has.
func (s *server) attach(sess *session, welcome wire.Message, runs []wire.Run) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil || s.closed || sess.generation != s.generation {
		return false
	}
	s.session, s.pong = sess, time.Now()
	// Again, under the lock: a pause after this reaches the session, and one before is in the list.
	if paused, err := s.paused.Names(wire.Namespace); err == nil {
		welcome.Paused = paused
	}
	for key, snap := range s.devices.List() {
		if key.Namespace == wire.Namespace {
			welcome.Devices = append(welcome.Devices, deviceOf(key.Name, snap))
		}
	}
	go s.write(sess)
	sess.out <- welcome
	// Still holding s.mu, so that nothing else reaches the session before the runs do.
	for i := range runs {
		select {
		case sess.out <- wire.Message{Type: wire.TypeRun, Unfinished: runs[i : i+1]}:
		case <-sess.gone:
			return true
		}
	}
	return true
}

func (s *server) detach(sess *session) {
	sess.end()
	s.mu.Lock()
	left := s.session == sess
	if left {
		s.session = nil
	}
	s.mu.Unlock()
	if left {
		s.pushed.set(nil)
		s.emit(api.Event{Time: time.Now(), Type: api.EventProcess, Message: "disconnected"})
	}
	close(sess.finished)
}

// disconnect ends the session, if there is one, and returns once its requests have all been carried
// out, so that the next process starts from everything the last one did. A connection that has yet
// to attach belonged to the process that's going, so it never will.
func (s *server) disconnect() {
	s.mu.Lock()
	sess := s.session
	s.generation++
	s.mu.Unlock()
	if sess != nil {
		sess.end()
		<-sess.finished
	}
}

// reconcile ends what's left of rules not named, their claims, Safety ones too, and their saved state,
// and the runs of flows not named. A flow can become another kind of rule, so a rule's name isn't a
// flow's. Nothing else would end any of it, because its code has gone. Only a deploy calls it, so a
// Safety rule goes only past the check that refuses that. An error means it should be tried again.
func (s *server) reconcile(rules, flows []string) error {
	if fault.Drop(fault.HostRemovals) {
		return errors.New("dropped by a fault point")
	}
	runs, err := s.journal.Runs()
	if err != nil {
		return fmt.Errorf("reading flow runs: %w", err)
	}
	var errs []error
	for rule, level := range s.engine.RuleClaims(wire.Namespace) {
		if slices.Contains(rules, rule) {
			continue
		}
		err := s.engine.EndRule(s.ctx, wire.Namespace, rule)
		if fault.Drop(fault.HostEndRule) {
			err = errors.New("dropped by a fault point")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("ending the claims of %s: %w", rule, err))
			continue
		}
		s.log.Info("Ended the claims of a removed rule", "rule", rule, "highest", level.String())
	}
	for _, saved := range []interface {
		Names(namespace string) ([]string, error)
		Delete(ctx context.Context, key ident.ID) error
	}{s.holds, s.fires, s.paused} {
		names, err := saved.Names(wire.Namespace)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, name := range names {
			if slices.Contains(rules, name) {
				continue
			}
			if err := saved.Delete(s.ctx, key(name)); err != nil {
				errs = append(errs, fmt.Errorf("forgetting the state of %s: %w", name, err))
				continue
			}
			s.log.Info("Forgot the saved state of a removed rule", "rule", name)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// Last, because a run's journal is the record of what it claimed.
	for _, run := range runs {
		if name := flow.FlowOf(run); name == "" || slices.Contains(flows, name) {
			continue
		}
		if err := s.journal.Delete(run); err != nil {
			errs = append(errs, fmt.Errorf("finishing flow run %s: %w", run, err))
			continue
		}
		s.log.Info("Dropped flow run; its flow has gone", "run", run)
	}
	return errors.Join(errs...)
}

// leftovers names every rule with anything on record: claims, saved state or an unfinished run.
func (s *server) leftovers() ([]string, error) {
	held := s.engine.RuleClaims(wire.Namespace)
	names := make([]string, 0, len(held))
	for rule := range held {
		names = append(names, rule)
	}
	holds, err := s.holds.Names(wire.Namespace)
	if err != nil {
		return nil, err
	}
	fires, err := s.fires.Names(wire.Namespace)
	if err != nil {
		return nil, err
	}
	runs, err := s.journal.Runs()
	if err != nil {
		return nil, err
	}
	names = append(append(names, holds...), fires...)
	for _, run := range runs {
		names = append(names, flow.FlowOf(run))
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// shutdown disconnects the session and refuses any more.
func (s *server) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.disconnect()
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

// recordCall logs a flow's outside call from its journal entry, for a replay to answer it with.
func (s *server) recordCall(run string, entry []byte) {
	if name, result, failure, ok := flow.CallIn(entry); ok {
		s.events.add(api.Event{
			Time: time.Now(), Type: api.EventCall, Rule: flow.FlowOf(run), Message: name, Value: result, Result: failure,
		})
	}
}

// emit passes an event to whoever's tailing, and on to the log. Neither waits.
func (s *server) emit(e api.Event) {
	s.feed.publish(e)
	s.events.add(e)
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
	s.emit(api.Event{
		Time: u.Time, Type: api.EventChange, Device: u.Device.Name, Capability: u.Capability,
		Value: u.Value, Previous: u.Previous,
	})
}

// Discovered implements driver.Sink.
func (s *server) Discovered(dev ident.ID, _ driver.Info) {
	snap, ok := s.devices.Get(dev)
	if !ok || dev.Namespace != wire.Namespace {
		return
	}
	d := deviceOf(dev.Name, snap)
	s.broadcast(wire.Message{Type: wire.TypeDiscovered, Discovered: &d})
}

// Seen tells your process a device reported, which a budget's reading needs to know how old it is.
func (s *server) Seen(dev ident.ID, t time.Time) {
	if dev.Namespace == wire.Namespace {
		s.broadcast(wire.Message{Type: wire.TypeSeen, Device: dev.Name, Time: t})
	}
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
		reply.Arbitrated, err = s.engine.Claim(s.ctx, key(m.Device), m.Capability, *m.Claim)
	case wire.TypeRelease:
		reply.Arbitrated, err = s.engine.ReleaseClaim(s.ctx, key(m.Device), m.Capability, m.Source)
	case wire.TypeReleaseAll:
		err = s.engine.ReleaseClaims(s.ctx, wire.Namespace, m.Source, m.Below)
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
	case wire.TypeSaveFire:
		err = s.fires.Put(s.ctx, key(m.Rule), m.Time)
	case wire.TypeJournal:
		fault.Hit(fault.HostJournalBefore)
		if !fault.Drop(fault.HostJournalAppend) {
			if reply.Seq, err = s.journal.Append(m.Run, m.Data); err == nil {
				s.recordCall(m.Run, m.Data)
			}
		}
		fault.Hit(fault.HostJournalAfter)
	case wire.TypeFinish:
		fault.Hit(fault.HostJournalBefore)
		err = s.journal.Delete(m.Run)
		fault.Hit(fault.HostJournalAfter)
	default:
		err = fmt.Errorf("unknown request %q", m.Type)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}

func key(name string) ident.ID {
	return ident.ID{Namespace: wire.Namespace, Name: name}
}

func deviceOf(name string, snap hub.Snapshot) wire.Device {
	d := wire.Device{
		Name: name, Room: snap.Room, Discovered: snap.Discovered,
		Capabilities: slices.Clone(snap.Info.Capabilities), DeviceTypes: slices.Clone(snap.DeviceTypes),
		Values: snap.Values, LastSeen: snap.LastSeen,
	}
	for _, c := range snap.Control {
		d.Arbitrated = append(d.Arbitrated, c.Capability)
	}
	return d
}
