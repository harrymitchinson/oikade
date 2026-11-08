package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// rulesInfo says what the running code's rules are: from the process, or if it's down, from the
// running version. known is false if neither has said.
func (s *apiServer) rulesInfo() (rules []wire.RuleInfo, known bool) {
	s.srv.mu.Lock()
	sess := s.srv.session
	s.srv.mu.Unlock()
	if sess != nil && len(sess.rules) > 0 {
		return sess.rules, true
	}
	v, ok := s.deploys.latest()
	if !ok || v.Manifest.Protocol == 0 {
		return nil, false
	}
	return v.Manifest.Rules, true
}

func (s *apiServer) status() api.Response {
	st := &api.Status{Devices: []api.Device{}, Rules: []api.Rule{}}
	if v, ok := s.deploys.latest(); ok {
		st.Version = v.ID
	}
	s.srv.mu.Lock()
	st.Process = s.srv.session != nil
	s.srv.mu.Unlock()
	var unimported []api.Unimported
	if s.srv.plugins != nil {
		unimported = s.srv.plugins.unimportedNow(false)
	}
	notImported := map[string]bool{}
	for _, u := range unimported {
		notImported[u.Device] = true
	}
	for dev, snap := range s.srv.devices.List() {
		d := api.Device{
			Name: dev.Name, Room: snap.Room, Online: snap.Online, Capabilities: []api.Capability{},
			DeviceTypes: slices.Clone(snap.DeviceTypes), PluginNotImported: notImported[dev.Name],
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
			c := api.Capability{Name: name}
			if v, ok := snap.Values[name]; ok {
				c.Value = v
			}
			if i := slices.IndexFunc(held, func(o control.Status) bool { return o.Capability == name }); i >= 0 {
				withControl(&c, held[i])
			}
			d.Capabilities = append(d.Capabilities, c)
		}
		st.Devices = append(st.Devices, d)
	}
	slices.SortFunc(st.Devices, func(a, b api.Device) int { return strings.Compare(a.Name, b.Name) })

	paused, err := s.srv.paused.Names()
	if err != nil {
		return api.Response{Error: fmt.Sprintf("reading paused rules: %v", err)}
	}
	rules, _ := s.rulesInfo()
	for _, r := range rules {
		st.Rules = append(st.Rules, api.Rule{Name: r.Name, Kind: r.Kind, Safety: r.Safety, Paused: slices.Contains(paused, r.Name)})
	}
	for _, name := range paused {
		if !slices.ContainsFunc(st.Rules, func(r api.Rule) bool { return r.Name == name }) {
			st.Rules = append(st.Rules, api.Rule{Name: name, Paused: true})
		}
	}
	slices.SortFunc(st.Rules, func(a, b api.Rule) int { return strings.Compare(a.Name, b.Name) })
	return api.Response{Status: st}
}

func withControl(c *api.Capability, held control.Status) {
	c.Level = held.Level
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
		claim := api.Claim{Source: o.Source, Level: o.Level, Value: rawValue(o.Value), Winner: o.Winner, Latched: o.Latched}
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
		return api.Response{Error: fmt.Sprintf("no device %q", req.Device)}
	}
	if !slices.Contains(snap.Info.Capabilities, req.Capability) {
		return api.Response{Error: fmt.Sprintf("%s has no %s", req.Device, req.Capability)}
	}
	if spec, ok := capability.Lookup(req.Capability); !ok || !spec.Writable {
		return api.Response{Error: fmt.Sprintf("%s is read-only", req.Capability)}
	}
	value, err := capability.Validate(req.Capability, req.Value)
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	if holder, _ := heldAtSafety(s.srv.engine, s.srv.devices, dev, req.Capability, value); holder != "" {
		return api.Response{Error: fmt.Sprintf("%s holds %s %s at Safety", holder, req.Device, req.Capability)}
	}
	// Before the write, so that the device's answer can't come first.
	events, stop := s.srv.feed.subscribe()
	defer stop()
	s.srv.engine.CommandWrote(dev, req.Capability, value)
	if err := s.srv.outside.Set(ctx, dev, req.Capability, value); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{Outcome: s.settled(ctx, dev, req.Capability, value, events)}
}

// settleWait is how long a set waits for the device to answer, and settleQuiet how long it then waits
// for nothing more to change, such as ExternalRevert writing it back.
const (
	settleWait  = 3 * time.Second
	settleQuiet = 500 * time.Millisecond
)

// settled waits for a set's write to settle, and returns the capability as it is then, or nil if the
// device hasn't answered. A device that already had the value sends no change, so it counts as having
// answered.
func (s *apiServer) settled(ctx context.Context, dev ident.ID, name string, value any, events <-chan api.Event) *api.Capability {
	has := func() bool {
		snap, ok := s.srv.devices.Get(dev)
		v, known := snap.Values[name]
		return ok && known && capability.Float(v) == capability.Float(value)
	}
	answered := has()
	var quiet <-chan time.Time
	if answered {
		quiet = time.After(settleQuiet)
	}
	wait := time.NewTimer(settleWait)
	defer wait.Stop()
loop:
	for {
		select {
		case e, ok := <-events:
			if !ok {
				events = nil // Fell behind: the timers end it.
				continue
			}
			if e.Type == api.EventChange && e.Device == dev.Name && e.Capability == name {
				answered = true
				quiet = time.After(settleQuiet)
			}
		case <-quiet:
			break loop
		case <-wait.C:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	snap, ok := s.srv.devices.Get(dev)
	if !ok || (!answered && !has()) {
		return nil
	}
	c := &api.Capability{Name: name, Value: snap.Values[name]}
	for _, held := range s.srv.engine.ControlOf(dev) {
		if held.Capability == name {
			withControl(c, held)
		}
	}
	return c
}

// heldAtSafety finds what holds a capability at Safety against value, if anything does: a write would
// only be written back. It returns who holds it and what they want. A claim counts from the moment the
// host starts, before there's an arbiter for it, unless the capability is known not to be arbitrated
// any more. Claims that can't be read count as Safety, with nothing to say what they want.
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
		return fmt.Errorf("%s holds %s", c.Source, dev.Name)
	}
	return s.setter.Set(ctx, dev, name, value)
}

// recordedSetter logs what your automations write, whether or not the device takes it: that's what
// the version did, and all a replay can compare with.
type recordedSetter struct {
	emit   func(api.Event)
	setter driver.Setter
}

// Set implements driver.Setter.
func (s recordedSetter) Set(ctx context.Context, dev ident.ID, name string, value any) error {
	s.emit(api.Event{Time: time.Now(), Type: api.EventWrite, Device: dev.Name, Capability: name, Value: value})
	return s.setter.Set(ctx, dev, name, value)
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
	i := slices.IndexFunc(rules, func(r wire.RuleInfo) bool { return r.Name == req.Rule })
	switch {
	case known && i < 0:
		return api.Response{Error: fmt.Sprintf("no rule %q", req.Rule)}
	case req.AllowSafety:
	case !known:
		// In an emergency a process may be down, which is no reason to guess it isn't a Safety rule.
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("can't tell what %s is: no process is connected", req.Rule)}
	case rules[i].Safety:
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("%s is a Safety rule, so pausing it stops that protection", req.Rule)}
	case s.srv.engine.RuleClaims()[req.Rule] == control.Safety:
		// As a deploy's check: a flow can make a Safety claim its declaration doesn't show, and a pause
		// would leave that claim with no run to release it.
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("%s holds a Safety claim, which a pause would strand", req.Rule)}
	}
	ended, kept := s.claimsOf(req.Rule)
	if err := s.srv.setPaused(ctx, req.Rule, true); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{Ended: ended, Kept: kept}
}

// claimsOf lists what a rule claims, as "device capability": the claims a pause ends, and the Safety
// ones it keeps.
func (s *apiServer) claimsOf(rule string) (ended, kept []string) {
	source := "Rule/" + rule
	for dev := range s.srv.devices.List() {
		for _, held := range s.srv.engine.ControlOf(dev) {
			for _, c := range held.Claims {
				if c.Source != source || c.Reverted {
					continue
				}
				if c.Level == control.NameSafety {
					kept = append(kept, dev.Name+" "+held.Capability)
				} else {
					ended = append(ended, dev.Name+" "+held.Capability)
				}
			}
		}
	}
	slices.Sort(ended)
	slices.Sort(kept)
	return ended, kept
}

func (s *apiServer) resume(ctx context.Context, req api.Request) api.Response {
	if _, ok := s.srv.paused.Get(key(req.Rule)); !ok {
		return api.Response{Error: fmt.Sprintf("%s isn't paused", req.Rule)}
	}
	if err := s.srv.setPaused(ctx, req.Rule, false); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
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
	s.log.Info("Changed rule pause", "rule", rule, "paused", paused)
	s.emit(api.Event{Time: time.Now(), Type: api.EventPause, Rule: rule, Message: what})
	if paused {
		if err := s.engine.ReleaseRule(ctx, key(rule)); err != nil {
			return errors.Join(errors.New("paused, but its claims didn't end"), err)
		}
	}
	return nil
}
