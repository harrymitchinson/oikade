package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
)

// rulesInfo says what the running code's rules are: from the process, or if it's down, from the
// running version. known is false if neither has said, unless no code has run since the host
// started and there's none to run, which has no rules.
func (s *apiServer) rulesInfo() (rules []wire.RuleInfo, known bool) {
	s.srv.mu.Lock()
	sess, attached := s.srv.session, s.srv.attached
	s.srv.mu.Unlock()
	if sess != nil && (len(sess.rules) > 0 || sess.ruleless) {
		return sess.rules, true
	}
	rules, known, none := s.deploys.rules()
	return rules, known || none && !attached
}

// unknownRules says why rulesInfo can't say what the rules are.
func (s *apiServer) unknownRules() string {
	if _, _, busy := s.deploys.state(); busy {
		return "a deploy is under way and no process has said"
	}
	return "no process is connected"
}

func (s *apiServer) status() api.Response {
	st := &api.Status{Devices: []api.Device{}, Rules: []api.Rule{}, Plugins: []api.Plugin{}}
	v, ok, busy := s.deploys.state()
	if ok {
		st.Home.Version = v.ID
	}
	st.Home.Deploying = busy
	s.srv.mu.Lock()
	st.Home.Process = s.srv.session != nil
	if !s.srv.since.IsZero() {
		since := s.srv.since
		st.Home.Since = &since
	}
	s.srv.mu.Unlock()
	var unimported []api.Unimported
	plugins := map[string]*api.Plugin{}
	if s.srv.plugins != nil {
		unimported = s.srv.plugins.unimportedNow(false)
		plugins = s.srv.plugins.listing()
	}
	notImported := map[string]bool{}
	for _, u := range unimported {
		notImported[u.Device] = true
	}
	for dev, snap := range s.srv.devices.List() {
		d := api.Device{
			Name: dev.Name, Ref: ident.DeviceRef(dev.Name).String(), Room: snap.Room, Online: snap.Online, Capabilities: []api.Capability{},
			DeviceTypes: slices.Clone(snap.DeviceTypes), PluginNotImported: notImported[dev.Name], Problem: snap.Info.Error,
		}
		if !snap.OfflineSince.IsZero() && snap.Online != nil && !*snap.Online {
			since := snap.OfflineSince
			d.OfflineSince = &since
		}
		if s.devices != nil {
			if r, ok := s.devices.registry.Get(dev.Name); ok {
				d.Declare, _ = registry.Undeclared(r)
				if name, ok := api.PluginOf(r); ok {
					d.Plugin = name
					if plugins[name] == nil {
						plugins[name] = &api.Plugin{Name: name}
					}
					plugins[name].Devices = append(plugins[name].Devices, dev.Name)
				}
			}
		}
		held := s.srv.engine.ControlOf(dev)
		names := slices.Clone(snap.Info.Capabilities)
		for name := range snap.Values {
			names = append(names, name)
		}
		for _, c := range held {
			names = append(names, c.Capability)
		}
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			c := api.Capability{Name: name, Ref: ident.CapabilityRef(dev.Name, name).String()}
			if v, ok := snap.Values[name]; ok {
				c.Value = v
			}
			if i := slices.IndexFunc(held, func(o control.Status) bool { return o.Capability == name }); i >= 0 {
				withControl(d.Name, &c, held[i])
			}
			d.Capabilities = append(d.Capabilities, c)
		}
		st.Devices = append(st.Devices, d)
	}
	slices.SortFunc(st.Devices, func(a, b api.Device) int { return strings.Compare(a.Name, b.Name) })
	for _, name := range slices.Sorted(maps.Keys(plugins)) {
		pl := plugins[name]
		pl.Ref = ident.NewRef(ident.Plugin, name).String()
		if pl.Devices == nil {
			pl.Devices = []string{}
		}
		slices.Sort(pl.Devices)
		st.Plugins = append(st.Plugins, *pl)
	}
	switch resp := s.matter(); {
	case resp.Matter != nil:
		st.Matter = resp.Matter
	case s.bridge != nil:
		// A bridge that can't say its codes is down for status, rather than turned off.
		st.Matter = &api.Matter{Problem: resp.Error, Fabrics: []api.Fabric{}}
	}

	paused, err := s.srv.paused.Names()
	if err != nil {
		return api.Response{Error: fmt.Sprintf("reading paused rules: %v", err)}
	}
	rules, _ := s.rulesInfo()
	s.srv.mu.Lock()
	var stopped, whyNot map[string]string
	if sess := s.srv.session; sess != nil {
		stopped, whyNot = maps.Clone(sess.stopped), maps.Clone(sess.whyNot)
	}
	s.srv.mu.Unlock()
	for _, r := range rules {
		rule := api.Rule{
			Name: r.Name, Ref: control.RuleRef(r.Name).String(), Kind: r.Kind, Safety: r.Safety,
			Paused: slices.Contains(paused, r.Name),
			Device: r.Device, Capability: r.Capability, Case: r.Case,
		}
		if r.Kind == wire.KindCase && !rule.Paused {
			s.withHold(&rule, stopped[r.Name])
			if !*rule.Active {
				rule.WhyNot = whyNot[r.Name]
			}
		}
		st.Rules = append(st.Rules, rule)
	}
	for _, name := range paused {
		if !slices.ContainsFunc(st.Rules, func(r api.Rule) bool { return r.Name == name }) {
			st.Rules = append(st.Rules, api.Rule{Name: name, Ref: control.RuleRef(name).String(), Paused: true})
		}
	}
	slices.SortFunc(st.Rules, func(a, b api.Rule) int { return strings.Compare(a.Name, b.Name) })
	return api.Response{Status: st}
}

// withHold adds a hold's state, as it last saved it, and why it stopped, if it did.
func (s *apiServer) withHold(r *api.Rule, stopped string) {
	saved, _ := s.srv.holds.Get(key(r.Name))
	active := saved.Active
	r.Active, r.Since, r.OnAt, r.OffAt = &active, saved.Since, saved.OnAt, saved.OffAt
	r.Latched, r.Stopped = saved.Latched, stopped
	if day, ok := saved.BudgetToday(); ok {
		r.Budget = &api.RuleBudget{
			Date: day.Date, TargetMinutes: day.TargetMinutes, PlannedMinutes: day.ScheduledMinutes, RunMinutes: day.RunMinutes,
		}
	}
}

// withControl adds who holds a device's capability, naming each of its decision's cases.
func withControl(device string, c *api.Capability, held control.Status) {
	c.Level = held.Level
	if held.Limit != "" {
		c.Limit = held.Limit
		if !held.LimitUntil.IsZero() {
			at := held.LimitUntil
			c.LimitUntil = &at
		}
	}
	if held.Level == "" {
		c.Level = control.NameDefault
	}
	if held.Value != nil {
		c.Wanted = rawValue(held.Value)
	}
	for _, o := range held.Claims {
		if o.Reverted {
			continue
		}
		claim := api.Claim{Source: control.SourceRef(o.Source).String(), Level: o.Level, Value: rawValue(o.Value), Winner: o.Winner, Latched: o.Latched}
		if name, ok := control.CaseOf(o.Source, device, held.Capability); ok {
			claim.Case = name
			if o.Winner {
				c.Case = name
			}
		}
		if !o.Expires.IsZero() {
			at := o.Expires
			claim.Expires = &at
		}
		c.Claims = append(c.Claims, claim)
	}
}

func rawValue(raw []byte) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// set writes a capability as a person would: an arbitrated one takes it as an outside change, so
// its settings decide how long that lasts. A Safety claim would only write it back, so that's
// refused here, where the command can say so.
func (s *apiServer) set(ctx context.Context, req api.Request) api.Response {
	dev := key(req.Device)
	snap, ok := s.srv.devices.Get(dev)
	if !ok {
		return api.Response{Error: noDevice(req.Device).Error()}
	}
	if !slices.Contains(snap.Info.Capabilities, req.Capability) {
		return api.Response{Error: fmt.Sprintf("%s has no %s: oikade status %s lists what it has",
			ident.DeviceRef(req.Device), req.Capability, req.Device)}
	}
	if spec, ok := capability.Lookup(req.Capability); !ok || !spec.Writable {
		return api.Response{Error: fmt.Sprintf("%s is read-only", req.Capability)}
	}
	value, err := capability.Validate(req.Capability, req.Value)
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	if holder, _ := heldAtSafety(s.srv.engine, s.srv.devices, dev, req.Capability, value); holder != "" {
		what := ident.CapabilityRef(req.Device, req.Capability)
		now := s.capabilityNow(dev, req.Capability)
		if _, ok := control.CaseOf(holder, req.Device, req.Capability); ok {
			return api.Response{
				Error:   fmt.Sprintf("%s: %s holds it, a safety line", what, control.SourceRef(holder)),
				Outcome: now,
			}
		}
		return api.Response{Error: fmt.Sprintf("%s holds %s at Safety", control.SourceRef(holder), what), Outcome: now}
	}
	// Before the write, so that the device's answer can't come first.
	events, stop := s.srv.feed.subscribe()
	defer stop()
	s.srv.engine.CommandWrote(dev, req.Capability, value)
	if err := s.srv.setter.Set(driver.WithWriter(ctx, control.SourceCommand), dev, req.Capability, value); err != nil {
		s.srv.engine.ForgetCommandWrite(dev, req.Capability)
		return api.Response{Error: err.Error()}
	}
	return api.Response{Outcome: s.settled(ctx, dev, req.Capability, value, events)}
}

// settleTimes are how long a set waits for the device to answer (wait), and then for nothing more
// to change, such as ExternalRevert writing it back (quiet).
type settleTimes struct {
	wait, quiet time.Duration
}

var setSettle = settleTimes{wait: 3 * time.Second, quiet: 500 * time.Millisecond}

// settled waits for a set's write to settle, and returns the capability as it is then, or nil if the
// device hasn't answered.
func (s *apiServer) settled(ctx context.Context, dev ident.ID, name string, value any, events <-chan api.Event) *api.Capability {
	has := func() bool {
		snap, ok := s.srv.devices.Get(dev)
		v, known := snap.Values[name]
		return ok && known && capability.Near(name, capability.Float(v), capability.Float(value))
	}
	if !awaitAnswer(ctx, events, dev.Name, name, value, has, setSettle) {
		return nil
	}
	return s.capabilityNow(dev, name)
}

// capabilityNow is a device's capability as it is, with its claims, or nil if the device has gone.
func (s *apiServer) capabilityNow(dev ident.ID, name string) *api.Capability {
	snap, ok := s.srv.devices.Get(dev)
	if !ok {
		return nil
	}
	c := &api.Capability{Name: name, Ref: ident.CapabilityRef(dev.Name, name).String(), Value: snap.Values[name]}
	for _, held := range s.srv.engine.ControlOf(dev) {
		if held.Capability == name {
			withControl(dev.Name, c, held)
		}
	}
	return c
}

// awaitAnswer waits for the device's answer to a write of value, then for t.quiet with nothing more
// changing, and says whether it answered. The answer is a report of value, allowing for the rounding
// drivers do: any other report, as from a lamp on its way through a fade, is the device catching up.
// A device that already has value sends no change, so it counts as having answered. After the
// answer, any change, such as a write-back, restarts the quiet.
func awaitAnswer(ctx context.Context, events <-chan api.Event, dev, name string, value any, has func() bool,
	t settleTimes,
) bool {
	answered := has()
	var quiet <-chan time.Time
	if answered {
		quiet = time.After(t.quiet)
	}
	wait := time.NewTimer(t.wait)
	defer wait.Stop()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				events = nil // Fell behind: the timers end it.
				continue
			}
			if e.Type != api.EventChange || e.Device != dev || e.Capability != name {
				continue
			}
			if answered || capability.Near(name, capability.Float(e.Value), capability.Float(value)) {
				answered = true
				quiet = time.After(t.quiet)
			}
		case <-quiet:
			return true
		case <-wait.C:
			return answered || has()
		case <-ctx.Done():
			return answered || has()
		}
	}
}

// heldAtSafety finds what holds a capability at Safety against value, if anything does: a write
// would only be written back. It returns who holds it and what they want. A claim counts from the
// moment the host starts, before there's an arbiter for it, unless the capability is known not to be
// arbitrated any more. Claims that can't be read count as Safety, with nothing to say what they want.
func heldAtSafety(eng *engine.Engine, devices *hub.Store, dev ident.ID, name string, value any) (string, any) {
	if c, ok := eng.UnreadableClaim(dev); ok {
		return c.Source, nil
	}
	for _, c := range eng.ControlOf(dev) {
		if c.Capability != name {
			continue
		}
		if holder := safetyHolder(c); c.Level == control.NameSafety && holder != "" && !sameValue(c, value) {
			return holder, rawValue(c.Value)
		}
		return "", nil
	}
	if snap, ok := devices.Get(dev); ok && !snap.Controls(name) {
		return "", nil
	}
	if c, ok := eng.SavedSafety(dev, name, capability.Float(value)); ok {
		return c.Source, engine.ValueOf(name, c.Value)
	}
	return "", nil
}

// claimedSetter refuses to write a device whose claims can't be read, whoever's asking: nothing else
// stops a rule, since the device has no arbiters to claim through.
type claimedSetter struct {
	engine *engine.Engine
	setter driver.Setter
}

// Set implements driver.Setter.
func (s claimedSetter) Set(ctx context.Context, dev ident.ID, name string, value any) error {
	if c, ok := s.engine.UnreadableClaim(dev); ok {
		return fmt.Errorf("%s holds %s", control.SourceRef(c.Source), dev.Name)
	}
	return s.setter.Set(ctx, dev, name, value)
}

// recordedSetter logs every write and whose it is, whether or not the device takes it: that's what
// the version did, and all a replay can compare with. Every write to a device goes through it.
type recordedSetter struct {
	emit   func(api.Event)
	setter driver.Setter
}

// Set implements driver.Setter.
func (s recordedSetter) Set(ctx context.Context, dev ident.ID, name string, value any) error {
	e := api.Event{Time: time.Now(), Type: api.EventWrite, Device: dev.Name, Capability: name, Value: value}
	if source := driver.Writer(ctx); source != "" {
		e.By = control.SourceRef(source).String()
	}
	s.emit(e)
	return s.setter.Set(ctx, dev, name, value)
}

// writtenByRule is ctx saying a write under it is the rule's that source names, as your process says
// of its own. Only a rule's counts: your process doesn't write for a controller or for oikade set.
func writtenByRule(ctx context.Context, source string) context.Context {
	if !strings.HasPrefix(source, control.SourceRule) {
		return ctx
	}
	return driver.WithWriter(ctx, source)
}

func safetyHolder(c control.Status) string {
	for _, o := range c.Claims {
		if o.Winner {
			return o.Source
		}
	}
	return ""
}

func sameValue(c control.Status, value any) bool {
	if c.Value == nil {
		return false
	}
	return capability.Float(rawValue(c.Value)) == capability.Float(value)
}

// pause stops a rule until it's resumed. It's on disk before this answers, and in force at once.
func (s *apiServer) pause(ctx context.Context, req api.Request) api.Response {
	rules, known := s.rulesInfo()
	rule, err := s.ruleKey(req.Rule, func() []wire.RuleInfo { return rules })
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	i := slices.IndexFunc(rules, func(r wire.RuleInfo) bool { return r.Name == rule })
	switch {
	case known && i < 0:
		return api.Response{Error: noRule(req.Rule).Error()}
	case req.AllowSafety:
	case !known:
		// In an emergency a process may be down, which is no reason to guess it isn't a Safety rule.
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("can't tell what %s is: %s", req.Rule, s.unknownRules())}
	case rules[i].Safety:
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("%s is a safety line, so pausing it stops that protection", req.Rule)}
	case s.srv.engine.RuleClaims()[rule] == control.Safety:
		// As a deploy's check: a rule an older version declared can hold a Safety claim this one's
		// declaration doesn't show, and a pause would leave that claim with no run to release it.
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("%s holds a Safety claim, which a pause would strand", req.Rule)}
	}
	ended, kept := s.claimsOf(rule)
	if err := s.srv.setPaused(ctx, rule, true); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{Ended: ended, Kept: kept}
}

// ruleKey is the key the host keeps a rule under, from the ref a pause or a resume names it by. A
// ref that doesn't parse is refused, with the rule's ref in the error if it's a rule's bare key.
// rules is asked for only when the ref doesn't parse and names no paused rule.
func (s *apiServer) ruleKey(ref string, rules func() []wire.RuleInfo) (string, error) {
	rule, err := control.RuleKey(ref)
	if err == nil {
		return rule, nil
	}
	if !strings.Contains(ref, "/") {
		// Not every kind of ref there is: a rule's is one of two.
		err = fmt.Errorf("%s isn't a rule's ref: give a case's, case/DEVICE/CAPABILITY/CASE, or a flow's, flow/FLOW", ref)
	}
	_, paused := s.srv.paused.Get(key(ref))
	if !paused && !slices.ContainsFunc(rules(), func(r wire.RuleInfo) bool { return r.Name == ref }) {
		return "", err
	}
	// A key from before names had a rule, such as a flow called "Good Morning", has no ref to give.
	if suggested := control.RuleRef(ref); control.SameRule(suggested.String(), ref) {
		return "", fmt.Errorf("%q isn't a ref: name the rule as %s", ref, suggested)
	}
	return "", err
}

// claimsOf lists the capabilities a rule claims, by their refs: the claims a pause ends, and the
// Safety ones it keeps.
func (s *apiServer) claimsOf(rule string) (ended, kept []string) {
	source := control.SourceRule + rule
	for dev := range s.srv.devices.List() {
		for _, held := range s.srv.engine.ControlOf(dev) {
			for _, c := range held.Claims {
				if c.Source != source || c.Reverted {
					continue
				}
				if c.Level == control.NameSafety {
					kept = append(kept, ident.CapabilityRef(dev.Name, held.Capability).String())
				} else {
					ended = append(ended, ident.CapabilityRef(dev.Name, held.Capability).String())
				}
			}
		}
	}
	slices.Sort(ended)
	slices.Sort(kept)
	return ended, kept
}

func (s *apiServer) resume(ctx context.Context, req api.Request) api.Response {
	rule, err := s.ruleKey(req.Rule, func() []wire.RuleInfo { rules, _ := s.rulesInfo(); return rules })
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	if _, ok := s.srv.paused.Get(key(rule)); !ok {
		if rules, known := s.rulesInfo(); known && !slices.ContainsFunc(rules, func(r wire.RuleInfo) bool { return r.Name == rule }) {
			return api.Response{Error: noRule(req.Rule).Error()}
		}
		return api.Response{Error: fmt.Sprintf("%s isn't paused", req.Rule)}
	}
	if err := s.srv.setPaused(ctx, rule, false); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
}

// noDevice is a device the host has no record of, as every answer says it.
func noDevice(name string) error {
	return fmt.Errorf("there's no %s: oikade list devices lists them", ident.DeviceRef(name))
}

// noRule is a rule the running version doesn't have, as every answer says it.
func noRule(ref string) error {
	return fmt.Errorf("there's no %s in the running version: oikade status --json lists its rules", ref)
}

// setPaused saves a rule's pause and tells the process. A paused rule's claims end, but for Safety
// ones: they end only by a release, so a pause can't reopen a valve.
func (s *server) setPaused(ctx context.Context, rule string, paused bool) error {
	m, what := wire.Message{Type: wire.TypeResume, Rule: rule}, "resumed"
	var err error
	if paused {
		m, what = wire.Message{Type: wire.TypePause, Rule: rule}, "paused"
		err = s.paused.Put(ctx, key(rule), time.Now())
	} else {
		// Here as well as in your process, which may not be running: a time that passed during a
		// pause isn't caught up.
		if _, timed := s.fires.Get(key(rule)); timed {
			err = s.fires.Put(ctx, key(rule), time.Now())
		}
		err = errors.Join(err, s.paused.Delete(ctx, key(rule)))
	}
	if err != nil {
		return fmt.Errorf("saving the pause: %w", err)
	}
	s.broadcast(m)
	s.log.Info("Changed rule pause", "rule", control.RuleRef(rule), "paused", paused)
	s.emit(api.Event{Time: time.Now(), Type: api.EventPause, Rule: rule, Message: what})
	if paused {
		if err := s.engine.ReleaseRule(ctx, key(rule)); err != nil {
			return errors.Join(errors.New("paused, but its claims didn't end"), err)
		}
	}
	return nil
}
