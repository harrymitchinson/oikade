package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
)

// ControlState is what a Device's arbiters remember, by capability.
type ControlState map[string]control.State

// ControlStates keeps ControlStates, by Device.
type ControlStates = States[ControlState]

// unreadableStates is States that can tell a state it can't read from one that isn't there.
type unreadableStates interface {
	Unreadable(key types.NamespacedName) error
}

// UnreadableClaims is who holds a device whose saved claims can't be read.
const UnreadableClaims = "an unreadable claim record"

// allLevels is a release of every claim, whatever its level.
const allLevels = control.LevelDefault

// Sources of claims that aren't Rules.
const (
	// SourceDevice is a change the runtime didn't write, as from a wall switch.
	SourceDevice = "Device"
	// SourceMatter is a change a Matter bridge announced writing, for a Matter controller such as
	// the Home app.
	SourceMatter = "Matter"
	// SourceCommand is a change the oikade command asked for.
	SourceCommand = "Command"
	// rulePrefix starts a Rule's source, Rule/<name>.
	rulePrefix = "Rule/"
)

// staleReport is how long after a write a report of a different value is taken as the device
// catching up, rather than an outside change.
const staleReport = 5 * time.Second

// bridgeMatch is how long after a bridge announces a write the device's change to that
// value is taken as the write. It allows for slow plugins and for a report held back by
// staleReport.
const bridgeMatch = 30 * time.Second

// bridgeTolerance is how far a reported number may be from a bridge's write and still match it.
// Drivers round on the way to and from the device: a light.level of 40 comes back from
// Zigbee2MQTT as 40.2. Bools are 0 and 1, so it can't confuse them.
const bridgeTolerance = 0.5

// bridgeWrite is a bridge's write, waiting for the device to report it.
type bridgeWrite struct {
	source string
	value  float64
	at     time.Time
}

// NewControlLevelGauge returns the gauge Engine.ControlLevel expects.
func NewControlLevelGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oikade_device_control_level",
		Help: "The winning level of an arbitrated capability: 0 default, 1 Normal, 2 Override, 3 Safety.",
	}, []string{labelNamespace, "device", "capability"})
}

// arb is one arbitrated capability. Use it with e.mu held.
type arb struct {
	spec homev1alpha1.CapabilityControl
	a    *control.Arbiter
	// d is the last decision.
	d control.Decision
	// writing is true while the decision is being written, and pending asks for another
	// decision when it ends.
	writing, pending bool
	// wrote is when the runtime last wrote the device, and retry when to try a failed write again.
	wrote, retry time.Time
	// next is when the decision can next change without new input. Zero means never.
	next time.Time
	// dropped is set when a report was ignored while a write settled, so the device's value is
	// looked at again once it has.
	dropped bool
	// unconfirmed is a write the device is yet to report. lost is one its driver's last process may
	// have taken with it, so that the next report is what the device is, not somebody changing it.
	// A write that's taken ends it.
	unconfirmed, lost bool
}

func ruleSource(name string) string { return rulePrefix + name }

func claimLevel(p Priority) control.Level {
	switch p {
	case PrioritySafety:
		return control.Safety
	case PriorityOverride:
		return control.Override
	}
	return control.Normal
}

// Arbitration decides arbitrated capabilities for an engine that runs none itself, as in your
// process, whose claims live in the host so they outlive it (OIK-143). Its methods may block, so the
// engine never calls them with its lock held.
type Arbitration interface {
	// Claim makes a claim, and reports false when the capability isn't arbitrated.
	Claim(ctx context.Context, dev types.NamespacedName, name string, c control.Claim) bool
	// ReleaseClaim ends a source's claim, and reports false when the capability isn't arbitrated.
	ReleaseClaim(ctx context.Context, dev types.NamespacedName, name, source string) bool
	// ReleaseClaims ends a source's claims below a level on every arbitrated capability in the
	// namespace. allLevels means all of them.
	ReleaseClaims(ctx context.Context, namespace, source string, below control.Level) error
}

// ControlChanged implements hub.ControlWatcher: Run looks at the device's arbiters again.
func (e *Engine) ControlChanged(dev types.NamespacedName) {
	if e.Arbitration != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.controlDue[dev] = true
	e.signal()
}

// Discovered implements driver.Sink: an arbitrated device that has reported can be written, now
// rather than whenever a write that failed was due to be tried again.
func (e *Engine) Discovered(dev types.NamespacedName, _ driver.Info) {
	if snap, ok := e.Store.Get(dev); ok && len(snap.Control) > 0 {
		e.mu.Lock()
		for _, ar := range e.arbs[dev] {
			ar.retry = time.Time{}
		}
		e.mu.Unlock()
		e.ControlChanged(dev)
	}
}

// DriverReturned says a driver has a new process. A write its last one took but hadn't reported may
// have died with it, so the device is what it last reported, and what the new process reports next
// isn't an outside change. Either way the arbiter writes again if that's wrong, once the device has
// reported: a report of what the store already holds changes nothing, so it can't wait for one.
func (e *Engine) DriverReturned(instance driver.InstanceKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for dev, arbs := range e.arbs {
		snap, ok := e.Store.Get(dev)
		if !ok || snap.Instance != instance {
			continue
		}
		for name, ar := range arbs {
			if !ar.unconfirmed {
				continue
			}
			ar.unconfirmed, ar.lost, ar.dropped = false, true, false
			if v, reported := snap.Values[name]; reported {
				ar.a.Wrote(capability.Float(v))
			}
		}
	}
}

// ensure creates or updates the device's arbiters from its control entries, and drops those
// whose entry has gone. Call with e.mu held.
func (e *Engine) ensure(dev types.NamespacedName) {
	snap, ok := e.Store.Get(dev)
	if !ok {
		// The device left the store, as when it moves between driver instances. Its arbiters go,
		// but their saved state stays for when it comes back.
		for name := range e.arbs[dev] {
			e.dropArb(dev, name, false)
		}
		return
	}
	arbs := e.arbs[dev]
	if arbs == nil {
		arbs = map[string]*arb{}
		e.arbs[dev] = arbs
	}
	var saved ControlState
	loaded := false
	for _, c := range snap.Control {
		old := arbs[c.Capability]
		if old != nil && equality.Semantic.DeepEqual(old.spec, c) {
			continue
		}
		settings, err := ControlSettings(c, e.Location)
		if err != nil {
			// The operator reports it on the Device; the capability stays unarbitrated.
			e.Log.Info("Ignored invalid control entry", "device", dev, "error", err.Error())
			e.dropArb(dev, c.Capability, true)
			continue
		}
		var st control.State
		if old != nil {
			st = old.a.State()
		} else {
			if !loaded {
				if err := e.unreadableClaims(dev); err != nil {
					// It may hold a Safety claim, so it stays, and no arbiter starts empty to save
					// over it. UnreadableClaim refuses writes in the meantime.
					e.Log.Error(err, "Refused writes to a device whose claims can't be read", "device", dev)
					return
				}
				saved, _ = e.Controls.Get(dev)
				loaded = true
			}
			st = saved[c.Capability]
		}
		a, err := control.New(settings, e.Clock, st)
		if err != nil {
			e.Log.Error(err, "Failed to start arbiter", "device", dev, "capability", c.Capability)
			e.dropArb(dev, c.Capability, true)
			continue
		}
		ar := &arb{spec: *c.DeepCopy(), a: a}
		if old != nil {
			// A write in progress finishes on the new arbiter.
			ar.writing, ar.pending, ar.wrote, ar.retry, ar.dropped = old.writing, old.pending, old.wrote, old.retry, old.dropped
			ar.unconfirmed, ar.lost = old.unconfirmed, old.lost
		}
		arbs[c.Capability] = ar
	}
	for name := range arbs {
		if !snap.Controls(name) {
			e.dropArb(dev, name, true)
		}
	}
	if len(arbs) == 0 {
		delete(e.arbs, dev)
	}
}

// dropArb forgets an arbiter. forget also drops its saved state, as when its control entry has
// gone. Call with e.mu held.
func (e *Engine) dropArb(dev types.NamespacedName, name string, forget bool) {
	if _, ok := e.arbs[dev][name]; !ok {
		return
	}
	delete(e.arbs[dev], name)
	if len(e.arbs[dev]) == 0 {
		delete(e.arbs, dev)
	}
	if e.ControlLevel != nil {
		e.ControlLevel.DeleteLabelValues(dev.Namespace, dev.Name, name)
	}
	if forget {
		e.markSave(dev)
	}
	e.reportControl(dev)
}

// markSave asks saveControls to save the device's arbiters. Call with e.mu held.
func (e *Engine) markSave(dev types.NamespacedName) {
	e.saveDue[dev] = true
	select {
	case e.saveWake <- struct{}{}:
	default:
	}
}

// syncControls brings the device's arbiters up to date with its control entries and decides
// each. Call with e.mu held.
func (e *Engine) syncControls(ctx context.Context, dev types.NamespacedName) {
	e.ensure(dev)
	for name, ar := range e.arbs[dev] {
		e.decide(ctx, dev, name, ar)
	}
}

// arbiter returns the capability's arbiter, or nil when it isn't arbitrated. Call with e.mu held.
func (e *Engine) arbiter(dev types.NamespacedName, name string) *arb {
	if ar := e.arbs[dev][name]; ar != nil {
		return ar
	}
	if snap, ok := e.Store.Get(dev); !ok || !snap.Controls(name) {
		return nil
	}
	e.ensure(dev)
	return e.arbs[dev][name]
}

// decide works out what the device should be and starts writing it. Call with e.mu held.
func (e *Engine) decide(ctx context.Context, dev types.NamespacedName, name string, ar *arb) {
	if ar.writing {
		ar.pending = true
		return
	}
	now := e.Clock.Now()
	snap, ok := e.Store.Get(dev)
	v, reported := snap.Values[name]
	switch st := ar.a.State(); {
	case ok && reported && !st.Known:
		// The device reported before the arbiter knew it, as on a first start.
		ar.a.Wrote(capability.Float(v))
	case ar.dropped && !now.Before(ar.wrote.Add(staleReport)):
		// A report ignored while the write settled may have been a real change.
		ar.dropped = false
		if f := capability.Float(v); reported && f != st.Value {
			e.recordOutside(dev, name, ar, f)
		}
	}
	d := ar.a.Decide()
	ar.d, ar.next = d, d.Next
	if ar.dropped {
		ar.next = earliestOf(ar.next, ar.wrote.Add(staleReport))
	}
	e.markSave(dev)
	e.reportControl(dev)
	if !d.Write {
		return
	}
	switch {
	case !ok || !snap.Discovered:
		// Discovered decides again.
		return
	case now.Before(ar.retry):
		ar.next = earliestOf(ar.next, ar.retry)
		return
	}
	ar.writing = true
	value := d.Value
	e.runs.Go(func() { e.writeControl(ctx, dev, name, ar, value) })
}

// earliestOf returns the earlier of two times, where zero means none.
func earliestOf(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

// writeControl writes a decision, records it, and decides again.
func (e *Engine) writeControl(ctx context.Context, dev types.NamespacedName, name string, ar *arb, value float64) {
	err := e.Setter.Set(ctx, dev, name, ValueOf(name, value))
	e.mu.Lock()
	defer e.mu.Unlock()
	ar.writing = false
	// An edited control entry replaced the arbiter meanwhile: the write is its.
	cur := e.arbs[dev][name]
	if cur == nil {
		return
	}
	cur.writing = false
	now := e.Clock.Now()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.Log.Error(err, "Failed to write arbitrated capability", "device", dev, "capability", name)
		cur.retry = now.Add(retryFailed)
	} else {
		cur.a.Wrote(value)
		cur.wrote, cur.retry = now, time.Time{}
		cur.unconfirmed, cur.lost = true, false
	}
	cur.pending = false
	e.decide(ctx, dev, name, cur)
	e.signal()
}

// ValueOf turns an arbiter value into the capability's value.
func ValueOf(name string, v float64) any {
	if spec, ok := capability.Lookup(name); ok && spec.Kind == homev1alpha1.CapabilityKindBool {
		return v != 0
	}
	return v
}

// outside records a reported value on an arbitrated capability. A value the runtime didn't
// write is an outside change. Call with e.mu held.
func (e *Engine) outside(ctx context.Context, u driver.Update) {
	ar := e.arbs[u.Device][u.Capability]
	if ar == nil {
		if !e.controlDue[u.Device] {
			return
		}
		// The device's arbiters are due to be set up: do it now, so this report counts.
		if ar = e.arbiter(u.Device, u.Capability); ar == nil {
			return
		}
	}
	v := capability.Float(u.Value)
	st := ar.a.State()
	switch {
	case !st.Known:
		// The first value seen is the device's, not a change.
		ar.a.Wrote(v)
	case v == st.Value:
		ar.unconfirmed, ar.lost = false, false
		return
	case ar.lost && !ar.writing:
		ar.lost = false
		ar.a.Wrote(v)
	case ar.writing || e.Clock.Since(ar.wrote) < staleReport:
		e.Log.V(1).Info("Ignored report while a write settles", "device", u.Device, "capability", u.Capability, "value", u.Value)
		ar.dropped = true
		if !ar.writing {
			ar.next = earliestOf(ar.next, ar.wrote.Add(staleReport))
			e.signal()
		}
		return
	default:
		e.recordOutside(u.Device, u.Capability, ar, v)
	}
	e.decide(ctx, u.Device, u.Capability, ar)
	// The decision may end later, as an Override does after overrideHold.
	e.signal()
}

func (e *Engine) recordOutside(dev types.NamespacedName, name string, ar *arb, v float64) {
	source := SourceDevice
	if w, ok := e.bridgeWrites[dev][name]; ok && w.matches(v, e.Clock.Now()) {
		// A report on the way, as while a dimmer ramps, leaves the write for the final value.
		e.forgetBridgeWrite(dev, name)
		source = w.source
	}
	if err := ar.a.Outside(source, v); err != nil {
		e.Log.Error(err, "Failed to record outside change", "device", dev, "capability", name, "source", source)
	}
}

// MatterWrote records that a Matter bridge is writing value to an arbitrated capability, so the
// device's change to it is recorded as from Matter rather than the device. Bridges announce a
// write before making it, and the bus delivers in order, so this comes before the change.
func (e *Engine) MatterWrote(dev types.NamespacedName, name string, value any) {
	e.bridgeWrote(SourceMatter, dev, name, value)
}

// ForgetMatterWrite undoes MatterWrote for a write that never reached the device, so the device's
// next report isn't taken for it.
func (e *Engine) ForgetMatterWrite(dev types.NamespacedName, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgetBridgeWrite(dev, name)
}

// CommandWrote records that the oikade command is writing value to an arbitrated capability, which
// counts as a person's change, like one from Matter.
func (e *Engine) CommandWrote(dev types.NamespacedName, name string, value any) {
	e.bridgeWrote(SourceCommand, dev, name, value)
}

func (e *Engine) bridgeWrote(source string, dev types.NamespacedName, name string, value any) {
	if !e.arbitrated(dev, name) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Clock.Now()
	// Writes nobody reported go once they're too old to match.
	for d, ws := range e.bridgeWrites {
		for n, w := range ws {
			if !now.Before(w.at.Add(bridgeMatch)) {
				e.forgetBridgeWrite(d, n)
			}
		}
	}
	w := bridgeWrite{source: source, value: capability.Float(value), at: now}
	if ar := e.arbs[dev][name]; ar != nil {
		if st := ar.a.State(); st.Known && w.matches(st.Value, now) {
			// The device already has it, so no change will come.
			e.forgetBridgeWrite(dev, name)
			return
		}
	}
	if e.bridgeWrites[dev] == nil {
		e.bridgeWrites[dev] = map[string]bridgeWrite{}
	}
	e.bridgeWrites[dev][name] = w
}

// matches reports whether a value reported at now is the write's.
func (w bridgeWrite) matches(v float64, now time.Time) bool {
	return math.Abs(w.value-v) <= bridgeTolerance && now.Before(w.at.Add(bridgeMatch))
}

// forgetBridgeWrite drops a bridge's write. Call with e.mu held.
func (e *Engine) forgetBridgeWrite(dev types.NamespacedName, name string) {
	delete(e.bridgeWrites[dev], name)
	if len(e.bridgeWrites[dev]) == 0 {
		delete(e.bridgeWrites, dev)
	}
}

// claim makes a Rule's claim on an arbitrated capability. It reports false when the capability
// isn't arbitrated, so the caller writes it as usual. A Rule that is no longer loaded, or a run
// that was cancelled, claims nothing.
func (e *Engine) claim(ctx context.Context, rule, dev types.NamespacedName, name string, c control.Claim) bool {
	if e.Arbitration != nil {
		return e.claimElsewhere(ctx, rule, dev, name, c)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ar := e.arbiter(dev, name)
	if ar == nil {
		return false
	}
	if ctx.Err() != nil || e.rules[rule] == nil {
		return true
	}
	e.makeClaim(ctx, dev, name, ar, c)
	return true
}

// claimElsewhere is claim for an engine whose claims live in Arbitration.
func (e *Engine) claimElsewhere(ctx context.Context, rule, dev types.NamespacedName, name string, c control.Claim) bool {
	if !e.arbitrated(dev, name) {
		return false
	}
	e.mu.Lock()
	loaded := e.rules[rule] != nil
	e.mu.Unlock()
	if ctx.Err() != nil || !loaded {
		return true
	}
	return e.Arbitration.Claim(ctx, dev, name, c)
}

// Claim makes a claim for a rule that runs elsewhere, as the host does for your process, and
// returns once it's saved, so a claim it reported survives a crash. ctx is the engine's, not the
// request's: the write it leads to outlives the request. It reports false when the capability isn't
// arbitrated.
func (e *Engine) Claim(ctx context.Context, dev types.NamespacedName, name string, c control.Claim) (bool, error) {
	e.mu.Lock()
	ar := e.arbiter(dev, name)
	if ar == nil {
		e.mu.Unlock()
		return false, nil
	}
	e.makeClaim(ctx, dev, name, ar, c)
	e.markSave(dev)
	e.mu.Unlock()
	return true, e.flushControls(ctx, dev)
}

// makeClaim gives the arbiter a claim and decides again. Call with e.mu held.
func (e *Engine) makeClaim(ctx context.Context, dev types.NamespacedName, name string, ar *arb, c control.Claim) {
	if err := ar.a.Claim(c); err != nil {
		e.Log.Error(err, "Failed to claim", "device", dev, "capability", name, "source", c.Source)
		return
	}
	e.decide(ctx, dev, name, ar)
	e.signal()
}

// releaseClaim ends a source's claim on an arbitrated capability. It reports false when the
// capability isn't arbitrated.
func (e *Engine) releaseClaim(ctx context.Context, dev types.NamespacedName, name, source string) bool {
	if e.Arbitration != nil {
		return e.Arbitration.ReleaseClaim(ctx, dev, name, source)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.releaseHere(ctx, dev, name, source)
}

// releaseHere ends a source's claim, and reports false when the capability isn't arbitrated. Call
// with e.mu held.
func (e *Engine) releaseHere(ctx context.Context, dev types.NamespacedName, name, source string) bool {
	ar := e.arbiter(dev, name)
	if ar == nil {
		return false
	}
	ar.a.Release(source)
	e.decide(ctx, dev, name, ar)
	e.markSave(dev)
	e.signal()
	return true
}

// ReleaseClaim ends a source's claim for a rule that runs elsewhere, and returns once that's saved.
// ctx is the engine's, as with Claim. It reports false when the capability isn't arbitrated.
func (e *Engine) ReleaseClaim(ctx context.Context, dev types.NamespacedName, name, source string) (bool, error) {
	e.mu.Lock()
	ok := e.releaseHere(ctx, dev, name, source)
	e.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, e.flushControls(ctx, dev)
}

// ReleaseClaims ends a source's claims below a level on every arbitrated capability in the
// namespace, for a rule that runs elsewhere, and returns once that's saved. ctx is the engine's, as
// with Claim.
func (e *Engine) ReleaseClaims(ctx context.Context, namespace, source string, below control.Level) error {
	e.mu.Lock()
	changed := e.releaseSource(ctx, namespace, source, below)
	e.mu.Unlock()
	if len(changed) == 0 {
		return nil
	}
	return e.flushControls(ctx, changed...)
}

// endClaims ends a source's claims below a level, here or in Arbitration. A failure there has ended
// the session, and the next process starts from what's on disk. Call without e.mu held.
func (e *Engine) endClaims(ctx context.Context, namespace, source string, below control.Level) error {
	if e.Arbitration != nil {
		return e.Arbitration.ReleaseClaims(ctx, namespace, source, below)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releaseSource(ctx, namespace, source, below)
	return nil
}

// ReleaseFlow ends a flow's claims, whichever of its runs made them, but for Safety ones. They end
// only by an explicit release, so a failed notification can't reopen a valve.
func (e *Engine) ReleaseFlow(ctx context.Context, key types.NamespacedName) error {
	return e.endClaims(ctx, key.Namespace, ruleSource(key.Name), control.Safety)
}

// ReleaseRule ends the claims of a rule that runs elsewhere, but for Safety ones, as ReleaseFlow does,
// and returns once that's saved. ctx is the engine's, as with Claim.
func (e *Engine) ReleaseRule(ctx context.Context, key types.NamespacedName) error {
	return e.ReleaseClaims(ctx, key.Namespace, ruleSource(key.Name), control.Safety)
}

// ForgetDevice ends what's on record for a device that has left the store for good: its arbiters,
// and the saved claims that would otherwise wait for the next device of that name. It returns once
// that's on disk.
func (e *Engine) ForgetDevice(ctx context.Context, dev types.NamespacedName) error {
	e.mu.Lock()
	e.ensure(dev)
	e.mu.Unlock()
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	if err := e.Controls.Delete(ctx, dev); err != nil {
		return err
	}
	delete(e.savedControls, dev)
	return nil
}

func (e *Engine) unreadableClaims(dev types.NamespacedName) error {
	if s, ok := e.Controls.(unreadableStates); ok {
		return s.Unreadable(dev)
	}
	return nil
}

// UnreadableClaim is the Safety claim a device's saved claims count as while they can't be read,
// because they might be one. It holds the whole device until they can be, or ForgetDevice clears them.
func (e *Engine) UnreadableClaim(dev types.NamespacedName) (control.Claim, bool) {
	if e.unreadableClaims(dev) == nil {
		return control.Claim{}, false
	}
	return control.Claim{Source: UnreadableClaims, Level: control.Safety}, true
}

// SavedClaims returns the live claims on record for a device with no arbiters, such as one the host
// hasn't heard from.
func (e *Engine) SavedClaims(dev types.NamespacedName) []control.Claim {
	if c, ok := e.UnreadableClaim(dev); ok {
		return []control.Claim{c}
	}
	saved, ok := e.Controls.Get(dev)
	if !ok {
		return nil
	}
	now := e.Clock.Now()
	var claims []control.Claim
	for _, st := range saved {
		for _, c := range st.Claims {
			if c.Expires.IsZero() || c.Expires.After(now) {
				claims = append(claims, c)
			}
		}
	}
	return claims
}

// SavedSafety finds a Safety claim on record against value, for a capability with no arbiter yet.
func (e *Engine) SavedSafety(dev types.NamespacedName, name string, value float64) (control.Claim, bool) {
	if c, ok := e.UnreadableClaim(dev); ok {
		return c, true
	}
	saved, ok := e.Controls.Get(dev)
	if !ok {
		return control.Claim{}, false
	}
	now := e.Clock.Now()
	for _, c := range saved[name].Claims {
		if c.Level == control.Safety && c.Value != value && (c.Expires.IsZero() || c.Expires.After(now)) {
			return c, true
		}
	}
	return control.Claim{}, false
}

// ensureDue loads the arbiters of devices the engine has yet to look at. They stay due, because
// that's still to do. Call with e.mu held.
func (e *Engine) ensureDue() {
	for dev := range e.controlDue {
		e.ensure(dev)
	}
}

// RuleClaims returns the rules holding claims in the namespace, each with the highest level it holds.
// It counts devices the engine has yet to look at, whose claims are still only on disk.
func (e *Engine) RuleClaims(namespace string) map[string]control.Level {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureDue()
	held := map[string]control.Level{}
	for dev, arbs := range e.arbs {
		if dev.Namespace != namespace {
			continue
		}
		for _, ar := range arbs {
			for _, c := range ar.a.State().Claims {
				if rule, ok := strings.CutPrefix(c.Source, rulePrefix); ok && c.Level >= held[rule] {
					held[rule] = c.Level
				}
			}
		}
	}
	for _, dev := range e.savedDevices(namespace) {
		for _, c := range e.SavedClaims(dev) {
			if rule, ok := strings.CutPrefix(c.Source, rulePrefix); ok && c.Level >= held[rule] {
				held[rule] = c.Level
			}
		}
	}
	return held
}

// savedDevices are the namespace's devices with claims on record and no arbiters, whose driver hasn't
// attached, so that the claims are only on disk. Call with e.mu held.
func (e *Engine) savedDevices(namespace string) []types.NamespacedName {
	listed, ok := e.Controls.(interface {
		Names(namespace string) ([]string, error)
	})
	if !ok {
		return nil
	}
	names, err := listed.Names(namespace)
	if err != nil {
		e.Log.Error(err, "Failed to list saved claims", "namespace", namespace)
		return nil
	}
	var devices []types.NamespacedName
	for _, name := range names {
		dev := types.NamespacedName{Namespace: namespace, Name: name}
		if len(e.arbs[dev]) == 0 {
			devices = append(devices, dev)
		}
	}
	return devices
}

// endSaved ends a source's claims that are only on disk. A device whose record can't be read is left
// alone: what's in it isn't known.
func (e *Engine) endSaved(ctx context.Context, namespace, source string) error {
	e.mu.Lock()
	devices := e.savedDevices(namespace)
	e.mu.Unlock()
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	var errs []error
	for _, dev := range devices {
		saved, ok := e.Controls.Get(dev)
		if !ok {
			continue
		}
		changed := false
		for name, st := range saved {
			kept := slices.DeleteFunc(slices.Clone(st.Claims), func(c control.Claim) bool { return c.Source == source })
			if len(kept) != len(st.Claims) {
				st.Claims, changed = kept, true
				saved[name] = st
			}
		}
		if changed {
			errs = append(errs, e.Controls.Put(ctx, dev, saved))
		}
	}
	return errors.Join(errs...)
}

// EndRule ends every claim of a rule that no longer exists, Safety ones included, as Remove does for
// one in this engine. It returns once that's saved. ctx is the engine's, as with Claim.
func (e *Engine) EndRule(ctx context.Context, namespace, rule string) error {
	e.mu.Lock()
	e.ensureDue()
	e.mu.Unlock()
	if err := e.ReleaseClaims(ctx, namespace, ruleSource(rule), allLevels); err != nil {
		return err
	}
	return e.endSaved(ctx, namespace, ruleSource(rule))
}

// Responsive returns once the engine's lock is free. A condition stuck in a loop holds that lock,
// so a heartbeat that waits for this stops, and the host replaces the process.
func (e *Engine) Responsive() {
	e.mu.Lock()
	defer e.mu.Unlock()
}

// releaseSource ends a source's claims below a level on every arbitrated capability in the
// namespace, and returns the devices that changed. Call with e.mu held.
func (e *Engine) releaseSource(ctx context.Context, namespace, source string, below control.Level) []types.NamespacedName {
	var changed []types.NamespacedName
	for dev, arbs := range e.arbs {
		if dev.Namespace != namespace {
			continue
		}
		for name, ar := range arbs {
			if slices.ContainsFunc(ar.a.State().Claims, func(c control.Claim) bool {
				return c.Source == source && (below == allLevels || c.Level < below)
			}) {
				ar.a.Release(source)
				e.decide(ctx, dev, name, ar)
				e.markSave(dev)
				changed = append(changed, dev)
			}
		}
	}
	e.signal()
	return changed
}

// arbitrated reports whether a device arbitrates a capability.
func (e *Engine) arbitrated(dev types.NamespacedName, name string) bool {
	snap, ok := e.Store.Get(dev)
	return ok && snap.Controls(name)
}

// reportControl reports the device's arbitrated capabilities, if they changed at a transition.
// Call with e.mu held.
func (e *Engine) reportControl(dev types.NamespacedName) {
	if e.ControlLevel != nil {
		for name, ar := range e.arbs[dev] {
			e.ControlLevel.WithLabelValues(dev.Namespace, dev.Name, name).Set(float64(ar.d.Level))
		}
	}
	out := e.controlOf(dev)
	prev, reported := e.reportedControl[dev]
	if !reported && len(out) == 0 {
		return
	}
	sig, _ := json.Marshal(out)
	if prev == string(sig) {
		return
	}
	e.reportedControl[dev] = string(sig)
	if e.OnControl != nil {
		e.OnControl(dev, out)
	}
}

// ControlOf says who holds each of a device's arbitrated capabilities, and why.
func (e *Engine) ControlOf(dev types.NamespacedName) []homev1alpha1.CapabilityControlStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureDue()
	return e.controlOf(dev)
}

// controlOf is ControlOf. Call with e.mu held.
func (e *Engine) controlOf(dev types.NamespacedName) []homev1alpha1.CapabilityControlStatus {
	out := make([]homev1alpha1.CapabilityControlStatus, 0, len(e.arbs[dev]))
	for name, ar := range e.arbs[dev] {
		out = append(out, controlStatus(name, ar.d))
	}
	slices.SortFunc(out, func(a, b homev1alpha1.CapabilityControlStatus) int {
		return strings.Compare(a.Capability, b.Capability)
	})
	return out
}

func levelName(l control.Level) homev1alpha1.ControlLevel {
	switch l {
	case control.Safety:
		return homev1alpha1.ControlLevelSafety
	case control.Override:
		return homev1alpha1.ControlLevelOverride
	case control.Normal:
		return homev1alpha1.ControlLevelNormal
	}
	return homev1alpha1.ControlLevelDefault
}

func jsonValue(name string, v float64) apiextensionsv1.JSON {
	raw, _ := json.Marshal(ValueOf(name, v))
	return apiextensionsv1.JSON{Raw: raw}
}

// controlStatus is a decision as Device status shows it.
func controlStatus(name string, d control.Decision) homev1alpha1.CapabilityControlStatus {
	v := jsonValue(name, d.Value)
	s := homev1alpha1.CapabilityControlStatus{Capability: name, Value: &v, Level: levelName(d.Level)}
	for _, c := range d.Claims {
		cc := homev1alpha1.ControlClaim{
			Source: c.Source, Level: levelName(c.Level), Value: jsonValue(name, c.Value),
			Latched: c.Latched, Winner: c.Source == d.Source,
		}
		if !c.Expires.IsZero() {
			cc.Expires = metav1.NewTime(c.Expires.Truncate(time.Second))
		}
		s.Claims = append(s.Claims, cc)
	}
	if r := d.Reverted; r != nil {
		s.Claims = append(s.Claims, homev1alpha1.ControlClaim{
			Source: r.Source, Level: homev1alpha1.ControlLevelOverride, Value: jsonValue(name, r.Value), Reverted: true,
		})
	}
	return s
}

// saveTimeout bounds the last save when the engine stops.
const saveTimeout = 5 * time.Second

// saveControls saves the arbiters' state of every device that changed. It runs until ctx ends,
// then saves what's left. A failed save is tried again.
func (e *Engine) saveControls(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
			defer cancel()
			e.saveDevices(last)
			return
		case <-e.saveWake:
		case <-retry:
		}
		retry = nil
		if failed := e.saveDevices(ctx); len(failed) > 0 {
			retry = time.After(retryFailed)
		}
	}
}

// flushControls saves what's marked for saving, and returns once it's saved. Claims from another
// process use it: its answer is a promise that the claim outlives a crash, which saveControls is
// too late to keep. It reports failure only for devices, or for any device if none are named, and
// leaves saveControls to try again.
func (e *Engine) flushControls(ctx context.Context, devices ...types.NamespacedName) error {
	failed := e.saveDevices(ctx)
	if len(failed) == 0 {
		return nil
	}
	select {
	case e.saveWake <- struct{}{}:
	default:
	}
	if len(devices) > 0 && !slices.ContainsFunc(devices, func(d types.NamespacedName) bool { return slices.Contains(failed, d) }) {
		return nil
	}
	return fmt.Errorf("couldn't save the arbiters of %d devices", len(failed))
}

// saveDevices saves the devices marked for saving, unless their state is as last saved, and
// returns those it couldn't save, marked for saving again. A device whose arbiters have all gone has its saved state
// deleted, unless it has left the store, as when it moves between driver instances.
func (e *Engine) saveDevices(ctx context.Context) []types.NamespacedName {
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	saved := e.savedControls
	e.mu.Lock()
	states := map[types.NamespacedName]ControlState{}
	for dev := range e.saveDue {
		if _, present := e.Store.Get(dev); !present && len(e.arbs[dev]) == 0 {
			continue
		}
		st := ControlState{}
		for name, ar := range e.arbs[dev] {
			st[name] = ar.a.State()
		}
		states[dev] = st
	}
	clear(e.saveDue)
	e.mu.Unlock()
	var failed []types.NamespacedName
	for dev, st := range states {
		raw, _ := json.Marshal(st)
		if saved[dev] == string(raw) {
			continue
		}
		var err error
		if len(st) == 0 {
			if _, ok := e.Controls.Get(dev); ok {
				err = e.Controls.Delete(ctx, dev)
			}
		} else {
			err = e.Controls.Put(ctx, dev, st)
		}
		if err != nil {
			if ctx.Err() == nil {
				e.Log.Error(err, "Failed to save control state", "device", dev)
			}
			failed = append(failed, dev)
			continue
		}
		saved[dev] = string(raw)
	}
	// With saveMu held, so nobody flushing can find nothing due while a save has failed.
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, dev := range failed {
		e.saveDue[dev] = true
	}
	return failed
}
