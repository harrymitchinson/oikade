package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/fault"
	"oikade.dev/internal/flow"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
)

const (
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
	// emitting orders the log and the feed: emit.
	emitting sync.Mutex
	// setter makes every write to a device, and logs whose it is.
	setter   driver.Setter
	holds    *engine.Stored[engine.HoldState]
	fires    *engine.Stored[time.Time]
	averages *engine.Stored[engine.AverageState]
	signals  *engine.Stored[engine.SignalState]
	journal  *store.Journal
	// paused are the rules paused, with when.
	paused *engine.Stored[time.Time]
	// feed is what oikade tail streams, events what oikade replay reads, and pushed your process's
	// latest metrics.
	feed   *feed
	events *eventLog
	pushed *pushed

	mu      sync.Mutex
	session *session
	// since is when the session last came or went, or the host started.
	since time.Time
	// attached is whether a process has attached since the host started.
	attached bool
	// closed refuses new sessions, once the host is stopping.
	closed bool
	pong   time.Time
	// generation counts disconnects.
	generation uint64

	// connected, if set, hears of each session. It must not block.
	connected func()
	plugins   *plugins
	// names say what the devices whose keys the host keeps are called, for what a person reads.
	names control.Names
	// settings, if set, takes up what a connecting process declares, and confirmedSettings replaces
	// them with what a version that has settled declares.
	settings, confirmedSettings func([]wire.DeviceControl) error
	// options, if set, takes up the Matter bridge's name a connecting process declares, and the
	// plugins it hides from Matter.
	options *homeOptions
	// deploying, if set, says a version is running that hasn't settled.
	deploying func() bool
	// declare, if set, takes up the devices a connecting process declares, and planDeclared says
	// what a version's would do.
	declare      func(decls []wire.DeviceDeclaration) error
	planDeclared func([]wire.DeviceDeclaration) (registry.Declared, error)
	// leavingSafety, if set, names the devices a version's declared rooms and types take out of
	// Safety rules.
	leavingSafety func(decls []wire.DeviceDeclaration, old, next []wire.RuleInfo) []string
	// declaring, if set, says what a version's declarations would change of the hub's devices, and
	// which devices they put in a room whose decision they don't take, under old and next control.
	declaring func(decls []wire.DeviceDeclaration, old, next []wire.DeviceControl) (members, []string, error)
	// attaching is held from a process's settings until it has attached: see open.
	attaching sync.Mutex

	// heldMu guards holding and held.
	heldMu sync.Mutex
	// holding is true while a version that hasn't settled runs, and held is every claim owed an end.
	holding bool
	held    map[heldRelease]struct{}
}

// heldRelease is a non-Safety claim a flow gave up under a version that hasn't settled. It ends when
// the version is confirmed, and stays if the version fails, so nothing is written to the device.
type heldRelease struct {
	Device     string `json:"device"`
	Capability string `json:"capability"`
	Source     string `json:"source"`
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
	// rules are what the process's hello said about its rules, and ruleless says it named none, so
	// that no rules is known rather than unsaid.
	rules    []wire.RuleInfo
	ruleless bool
	// reads are the rooms each hold has read, from RoomReads, stopped why each hold couldn't tell
	// whether it should be active, from Stopped, and whyNot why each case is inactive, from WhyNot.
	// Each is by rule, and the server's mu guards them.
	reads   map[string][]engine.RoomRead
	stopped map[string]string
	whyNot  map[string]string
	// signals are the process's signals, by name, as its hello said, and said what each last said of
	// itself, from SignalWhy, which the server's mu guards.
	signals []string
	said    map[string]signalSaid
}

// signalSaid is what a signal's conditions say, and the devices it leaves out as dead.
type signalSaid struct {
	why  string
	dead []string
}

func (s *session) end() {
	s.once.Do(func() {
		_ = s.conn.Close()
		close(s.gone)
	})
}

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
		generation: generation, rules: hello.Info, ruleless: len(hello.Rules) == 0, signals: hello.Signals,
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
			if err := s.pushed.set(m.Metrics); err != nil {
				s.log.Error("Refused rule metrics from your process", "err", err)
			}
			continue
		case wire.TypeRoomReads:
			s.mu.Lock()
			if sess.reads == nil {
				sess.reads = map[string][]engine.RoomRead{}
			}
			sess.reads[m.Rule] = m.Reads
			s.mu.Unlock()
			continue
		case wire.TypeStopped:
			s.mu.Lock()
			if sess.stopped == nil {
				sess.stopped = map[string]string{}
			}
			if m.Stopped == "" {
				delete(sess.stopped, m.Rule)
			} else {
				sess.stopped[m.Rule] = m.Stopped
			}
			s.mu.Unlock()
			continue
		case wire.TypeSignalWhy:
			s.mu.Lock()
			if sess.said == nil {
				sess.said = map[string]signalSaid{}
			}
			sess.said[m.Rule] = signalSaid{why: m.Why, dead: m.Dead}
			s.mu.Unlock()
			continue
		case wire.TypeWhyNot:
			s.mu.Lock()
			if sess.whyNot == nil {
				sess.whyNot = map[string]string{}
			}
			if m.WhyNot == "" {
				delete(sess.whyNot, m.Rule)
			} else {
				sess.whyNot[m.Rule] = m.WhyNot
			}
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
		AverageStates: map[string]engine.AverageState{}, SignalStates: map[string]engine.SignalState{},
	}
	for _, name := range hello.Averages {
		if st, ok := s.averages.Get(key(name)); ok {
			welcome.AverageStates[name] = st
		}
	}
	for _, name := range hello.Signals {
		if st, ok := s.signals.Get(key(name)); ok {
			welcome.SignalStates[name] = st
		}
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
	if welcome.Paused, err = s.paused.Names(); err != nil {
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
	if !s.turningAway(sess.generation) {
		// Before the settings, so the hub has the devices they're for.
		if s.declare != nil {
			if err := s.declare(hello.Inventory); err != nil {
				s.log.Error("Failed to apply the home's device declarations", "err", err)
				return "the host couldn't apply the home's device declarations: " + err.Error()
			}
		}
		if err := s.takeSettings(hello.Control); err != nil {
			s.log.Error("Failed to save the home's device settings", "err", err)
			return "the host couldn't save the home's device settings"
		}
		if err := s.takeMatterName(hello.MatterName); err != nil {
			s.log.Error("Failed to save the Matter bridge's name", "err", err)
			return "the host couldn't save the Matter bridge's name"
		}
		// A version that hasn't settled may fail, and its hiding would cost controllers the devices'
		// rooms and scenes for nothing, so a deploy hides them once it's confirmed.
		if s.deploying == nil || !s.deploying() {
			if err := s.takeHidden(hello.HiddenFromMatter); err != nil {
				s.log.Error("Failed to save the plugins hidden from Matter", "err", err)
				return "the host couldn't save the plugins hidden from Matter"
			}
		}
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

// turningAway says attach is going to turn a process away, so that it has no say. Call with
// attaching held.
func (s *server) turningAway(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session != nil || s.closed || generation != s.generation
}

func (s *server) takeSettings(settings []wire.DeviceControl) error {
	if s.settings == nil {
		return nil
	}
	return s.settings(settings)
}

func (s *server) takeMatterName(name string) error {
	if s.options == nil {
		return nil
	}
	return s.options.takeMatterName(name)
}

func (s *server) takeHidden(hidden []wire.HiddenPlugin) error {
	if s.options == nil {
		return nil
	}
	return s.options.takeHidden(hidden)
}

// takenOffMatter names the devices on the bridge that a version's home hides from Matter and the
// running one doesn't, which a deploy of it takes off.
func (s *server) takenOffMatter(m wire.Manifest) []string {
	if s.options == nil || s.plugins == nil {
		return nil
	}
	now := s.options.hiddenFromMatter()
	var out []string
	for _, d := range s.plugins.known.registry.List().Devices {
		if api.HiddenFromMatter(m.HiddenFromMatter, d) && !api.HiddenFromMatter(now, d) {
			out = append(out, d.Name)
		}
	}
	slices.Sort(out)
	return out
}

// confirmSettings makes the device settings exactly what a settled version declares.
func (s *server) confirmSettings(settings []wire.DeviceControl) error {
	if s.confirmedSettings == nil {
		return nil
	}
	return s.confirmedSettings(settings)
}

// registry is the host's record of devices, or nil without one.
func (s *server) registry() *registry.Registry {
	if s == nil || s.plugins == nil || s.plugins.known == nil {
		return nil
	}
	return s.plugins.known.registry
}

// safetyHeld says a Safety claim holds a device's capability, by the device's key.
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
	s.session, s.pong, s.attached = sess, time.Now(), true
	s.since = s.pong
	// Again, under the lock: a pause after this reaches the session, and one before is in the list.
	if paused, err := s.paused.Names(); err == nil {
		welcome.Paused = paused
	}
	for key, snap := range s.devices.List() {
		welcome.Devices = append(welcome.Devices, deviceOf(key.Name, snap))
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
		s.session, s.since = nil, time.Now()
	}
	s.mu.Unlock()
	if left {
		_ = s.pushed.set(nil)
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

// reconcile ends what's left of rules not named, their claims, Safety ones too, and their saved
// state, and the runs of flows not named. A flow can become another kind of rule, so a rule's name
// isn't a flow's. Nothing else would end any of it, because its code has gone. Only a deploy calls
// it, so a Safety rule goes only past the check that refuses that. An error means it should be tried
// again.
func (s *server) reconcile(rules, flows []string) error {
	if fault.Drop(fault.HostRemovals) {
		return errors.New("dropped by a fault point")
	}
	runs, err := s.journal.Runs()
	if err != nil {
		return fmt.Errorf("reading flow runs: %w", err)
	}
	var errs []error
	for rule, level := range s.engine.RuleClaims() {
		if slices.Contains(rules, rule) {
			continue
		}
		err := s.engine.EndRule(s.ctx, rule)
		if fault.Drop(fault.HostEndRule) {
			err = errors.New("dropped by a fault point")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("ending the claims of %s: %w", s.names.RuleRef(rule), err))
			continue
		}
		s.log.Info("Ended the claims of a removed rule", "rule", s.names.RuleRef(rule), "highest", level.String())
	}
	for _, saved := range []interface {
		Names() ([]string, error)
		Delete(ctx context.Context, key ident.ID) error
	}{s.holds, s.fires, s.paused} {
		names, err := saved.Names()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, name := range names {
			if slices.Contains(rules, name) {
				continue
			}
			if err := saved.Delete(s.ctx, key(name)); err != nil {
				errs = append(errs, fmt.Errorf("forgetting the state of %s: %w", s.names.RuleRef(name), err))
				continue
			}
			s.log.Info("Forgot the saved state of a removed rule", "rule", s.names.RuleRef(name))
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

// startHolding holds back what flows release from here on, until the version is confirmed or fails.
func (s *server) startHolding() {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	s.holding = true
	if s.held == nil {
		s.held = map[heldRelease]struct{}{}
	}
}

// closeHolds stops holding, so a release from here on is carried out rather than lost, and returns
// every claim owed an end, for the confirmation to record. What's owed stays, for endReleased.
func (s *server) closeHolds() []heldRelease {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	s.holding = false
	return slices.SortedFunc(maps.Keys(s.held), func(a, b heldRelease) int {
		return cmp.Or(cmp.Compare(a.Device, b.Device), cmp.Compare(a.Capability, b.Capability), cmp.Compare(a.Source, b.Source))
	})
}

// reopenHolds goes back to holding, for a confirmation that couldn't be saved.
func (s *server) reopenHolds() {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	s.holding = true
}

// resetHolds stops holding, and leaves owed only what's on record, forgetting what a failed version
// released, which never ended.
func (s *server) resetHolds(recorded []heldRelease) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	s.holding = false
	s.held = make(map[heldRelease]struct{}, len(recorded))
	for _, r := range recorded {
		s.held[r] = struct{}{}
	}
}

// hold reports whether a release was held back. Only a flow's release is, which is the only one
// over the socket that stops short of Safety claims. Everything else ends what the running code
// decided to.
func (s *server) hold(source string, below control.Level) bool {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	if !s.holding || below != control.Safety {
		return false
	}
	for _, c := range s.engine.SourceClaims(source, below) {
		s.held[heldRelease{Device: c.Device, Capability: c.Capability, Source: source}] = struct{}{}
	}
	return true
}

// claim makes a claim for your process. A flow claiming what it released means what it does now is
// what counts, so that claim is no longer owed an end, unless the arbiter refused it and the old one
// stands. heldMu is held throughout, so endReleased can't end a claim as it's made.
func (s *server) claim(device, capability string, c control.Claim) (bool, error) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	arbitrated, err := s.engine.Claim(s.ctx, key(device), capability, c)
	r := heldRelease{Device: device, Capability: capability, Source: c.Source}
	if _, owed := s.held[r]; owed {
		if now, ok := s.engine.ClaimOf(key(device), capability, c.Source); ok && now.Value == c.Value {
			delete(s.held, r)
		}
	}
	return arbitrated, err
}

// release ends a claim for your process, which is then no longer owed an end.
func (s *server) release(device, capability, source string) (bool, error) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	delete(s.held, heldRelease{Device: device, Capability: capability, Source: source})
	return s.engine.ReleaseClaim(s.ctx, key(device), capability, source)
}

// endReleased ends the claims owed an end. An error means it should be tried again, and what ended
// stays ended. A device that has gone has no claim to end here.
func (s *server) endReleased() error {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	var errs []error
	for r := range s.held {
		ended, err := s.engine.ReleaseClaim(s.ctx, key(r.Device), r.Capability, r.Source)
		if err != nil {
			errs = append(errs, fmt.Errorf("ending the claim of %s on %s: %w", r.Source, r.Device, err))
			continue
		}
		delete(s.held, r)
		if ended {
			s.log.Info("Ended the claim of a flow run", "source", s.names.SourceRef(r.Source), "device", s.names.Device(r.Device), "capability", r.Capability)
		} else {
			s.log.Info("Found no claim of a flow run to end", "source", s.names.SourceRef(r.Source), "device", s.names.Device(r.Device), "capability", r.Capability)
		}
	}
	return errors.Join(errs...)
}

// leftovers names every rule with anything on record: claims, saved state or an unfinished run.
func (s *server) leftovers() ([]string, error) {
	held := s.engine.RuleClaims()
	names := make([]string, 0, len(held))
	for rule := range held {
		names = append(names, rule)
	}
	holds, err := s.holds.Names()
	if err != nil {
		return nil, err
	}
	fires, err := s.fires.Names()
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

func (s *server) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.disconnect()
}

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
		s.emit(api.Event{
			Time: time.Now(), Type: api.EventCall, Rule: flow.FlowOf(run), Message: name, Value: result, Result: failure,
		})
	}
}

// emit passes an event on to the log, and to whoever's tailing. Neither waits. They take it together,
// so that a tail that starts with the log's past has every event once: tailWithPast.
func (s *server) emit(e api.Event) {
	if e.Ref == "" {
		e.Ref = eventRef(e)
	}
	s.emitting.Lock()
	defer s.emitting.Unlock()
	s.events.add(e)
	s.feed.publish(e)
}

// tailWithPast starts a tail that the log's past comes before: the events from now on, how to stop,
// and the place of the last record in the log from before them. The log's events up to there, then
// these, are every event once.
func (s *server) tailWithPast() (events <-chan api.Event, stop func(), upTo uint64, err error) {
	upTo, err = s.events.flushedAt(func(take func()) {
		s.emitting.Lock()
		defer s.emitting.Unlock()
		events, stop = s.feed.subscribe()
		take()
	})
	if err != nil {
		stop()
	}
	return events, stop, upTo, err
}

// eventRef is the ref of what an event is about: the capability changed, written or refused, the
// device, the rule run or paused, or the bridge. A plugin's event comes with its ref.
func eventRef(e api.Event) string {
	switch {
	case e.Type == api.EventMatter:
		return ident.NewRef(ident.Matter).String()
	case e.Device != "" && e.Capability != "":
		return ident.CapabilityRef(e.Device, e.Capability).String()
	case e.Device != "":
		return ident.DeviceRef(e.Device).String()
	case e.Rule != "":
		return control.RuleRef(e.Rule).String()
	}
	return ""
}

// Changed implements driver.Sink. A report goes to your process as one message, so it takes in
// every value before a rule looks.
func (s *server) Changed(changes ...driver.Update) {
	for len(changes) > 0 {
		dev := changes[0].Device
		n := 1
		for n < len(changes) && changes[n].Device == dev {
			n++
		}
		report := changes[:n]
		changes = changes[n:]
		m := wire.Message{Type: wire.TypeChange, Device: dev.Name, Time: report[0].Time}
		for _, u := range report {
			m.Changes = append(m.Changes, wire.Change{Capability: u.Capability, Value: u.Value, Previous: u.Previous})
		}
		s.broadcast(m)
		for _, u := range report {
			s.emit(api.Event{
				Time: u.Time, Type: api.EventChange, Device: u.Device.Name, Capability: u.Capability,
				Value: u.Value, Previous: u.Previous,
			})
		}
	}
}

// went puts a device going offline, or coming back, in the event log, and tells your process. One
// going offline has its driver's problem with it, if it has one. One coming back doesn't: its driver
// may not have said yet that the problem has gone.
func (s *server) went(dev ident.ID, online bool) {
	s.broadcast(wire.Message{Type: wire.TypeAvailability, Device: dev.Name, Online: &online})
	e := api.Event{Time: time.Now(), Type: api.EventAvailability, Device: dev.Name, Message: api.AvailabilityOnline}
	if !online {
		e.Message = api.AvailabilityOffline
		if snap, ok := s.devices.Get(dev); ok {
			e.Result = snap.Info.Error
		}
	}
	s.emit(e)
}

// Discovered implements driver.Sink.
func (s *server) Discovered(dev ident.ID, _ driver.Info) {
	snap, ok := s.devices.Get(dev)
	if !ok {
		return
	}
	d := deviceOf(dev.Name, snap)
	s.broadcast(wire.Message{Type: wire.TypeDiscovered, Discovered: &d})
}

// Seen tells your process a device reported, which a budget's reading needs to know how old it is.
func (s *server) Seen(dev ident.ID, t time.Time) {
	s.broadcast(wire.Message{Type: wire.TypeSeen, Device: dev.Name, Time: t})
}

func (s *server) answer(m wire.Message) wire.Message {
	reply := wire.Message{Type: wire.TypeReply, ID: m.ID}
	var err error
	switch m.Type {
	case wire.TypeClaim:
		if m.Claim == nil {
			err = errors.New("a claim needs one")
			break
		}
		reply.Arbitrated, err = s.claim(m.Device, m.Capability, *m.Claim)
	case wire.TypeRelease:
		reply.Arbitrated, err = s.release(m.Device, m.Capability, m.Source)
	case wire.TypeReleaseAll:
		if !s.hold(m.Source, m.Below) {
			err = s.engine.ReleaseClaims(s.ctx, m.Source, m.Below)
		}
	case wire.TypeSet:
		err = s.setter.Set(writtenByRule(s.ctx, m.Source), key(m.Device), m.Capability, m.Value)
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
	case wire.TypeSaveAverage:
		if m.Average == nil {
			err = errors.New("saving an average needs its state")
			break
		}
		err = s.averages.Put(s.ctx, key(m.Rule), *m.Average)
	case wire.TypeSaveSignal:
		if m.Signal == nil {
			err = errors.New("saving a signal needs its state")
			break
		}
		was, saved := s.signals.Get(key(m.Rule))
		if err = s.signals.Put(s.ctx, key(m.Rule), *m.Signal); err == nil && saved && was.Value != m.Signal.Value {
			// When it changed, as a save can wait behind another.
			at := m.Signal.Since
			if at.IsZero() {
				at = time.Now()
			}
			s.emit(api.Event{
				Time: at, Type: api.EventSignal, Ref: ident.NewRef(ident.Signal, m.Rule).String(),
				Value: m.Signal.Value, Previous: was.Value,
			})
		}
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
	return ident.ID{Name: name}
}

func deviceOf(key string, snap hub.Snapshot) wire.Device {
	d := wire.Device{
		Key: key, Name: snap.DisplayName, Room: snap.Room, Discovered: snap.Discovered,
		Capabilities: slices.Clone(snap.Info.Capabilities), DeviceTypes: slices.Clone(snap.DeviceTypes),
		Values: snap.Values, LastSeen: snap.LastSeen, Online: snap.Online,
	}
	for _, c := range snap.Control {
		d.Arbitrated = append(d.Arbitrated, c.Capability)
	}
	return d
}
