package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
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
	for dev, snap := range s.srv.devices.List() {
		if dev.Namespace != wire.Namespace {
			continue
		}
		d := api.Device{Name: dev.Name, Room: snap.Room, Online: snap.Online, Capabilities: []api.Capability{}}
		held := s.srv.engine.ControlOf(dev)
		names := slices.Clone(snap.Info.Capabilities)
		for name := range snap.Values {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			c := api.Capability{Name: name}
			if v, ok := snap.Values[name]; ok {
				c.Value = v
			}
			if i := slices.IndexFunc(held, func(o homev1alpha1.CapabilityControlStatus) bool { return o.Capability == name }); i >= 0 {
				withControl(&c, held[i])
			}
			d.Capabilities = append(d.Capabilities, c)
		}
		st.Devices = append(st.Devices, d)
	}
	slices.SortFunc(st.Devices, func(a, b api.Device) int { return strings.Compare(a.Name, b.Name) })

	paused, err := s.srv.paused.Names(wire.Namespace)
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

func withControl(c *api.Capability, held homev1alpha1.CapabilityControlStatus) {
	c.Level = string(held.Level)
	if held.Level == "" {
		c.Level = string(homev1alpha1.ControlLevelDefault)
	}
	if held.Value != nil {
		c.Wanted = rawValue(held.Value.Raw)
	}
	for _, o := range held.Claims {
		if o.Reverted {
			continue
		}
		claim := api.Claim{Source: o.Source, Level: string(o.Level), Value: rawValue(o.Value.Raw), Winner: o.Winner, Latched: o.Latched}
		if !o.Expires.IsZero() {
			at := o.Expires.Time
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
	for _, c := range s.srv.engine.ControlOf(dev) {
		if c.Capability != req.Capability || c.Level != homev1alpha1.ControlLevelSafety {
			continue
		}
		if holder := safetyHolder(c); holder != "" && !sameValue(c, value) {
			return api.Response{Error: fmt.Sprintf("%s holds %s %s at Safety", holder, req.Device, req.Capability)}
		}
	}
	s.srv.engine.CommandWrote(dev, req.Capability, value)
	if err := s.srv.setter.Set(ctx, dev, req.Capability, value); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
}

func safetyHolder(c homev1alpha1.CapabilityControlStatus) string {
	for _, o := range c.Claims {
		if o.Winner {
			return o.Source
		}
	}
	return ""
}

func sameValue(c homev1alpha1.CapabilityControlStatus, value any) bool {
	if c.Value == nil {
		return false
	}
	return capability.Float(rawValue(c.Value.Raw)) == capability.Float(value)
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
	case s.srv.engine.RuleClaims(wire.Namespace)[req.Rule] == control.Safety:
		// As a deploy's check: a flow can make a Safety claim its declaration doesn't show, and a pause
		// would leave that claim with no run to release it.
		return api.Response{NeedsAllowSafety: true, Error: fmt.Sprintf("%s holds a Safety claim, which a pause would strand", req.Rule)}
	}
	if err := s.srv.setPaused(ctx, req.Rule, true); err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
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
	s.feed.publish(api.Event{Time: time.Now(), Type: api.EventPause, Rule: rule, Message: what})
	if paused {
		if err := s.engine.ReleaseRule(ctx, key(rule)); err != nil {
			return errors.Join(errors.New("paused, but its claims didn't end"), err)
		}
	}
	return nil
}
