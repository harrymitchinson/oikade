package engine

import (
	"context"
	"encoding/json"
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

// Sources of claims that aren't Rules.
const (
	// SourceDevice is a change the runtime didn't write, as from a wall switch.
	SourceDevice = "Device"
	// rulePrefix starts a Rule's source, Rule/<name>.
	rulePrefix = "Rule/"
)

// staleReport is how long after a write a report of a different value is taken as the device
// catching up, rather than an outside change.
const staleReport = 5 * time.Second

// NewControlLevelGauge returns the gauge Engine.ControlLevel expects.
func NewControlLevelGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oikade_device_control_level",
		Help: "The winning level of an arbitrated capability: 0 default, 1 Normal, 2 Override, 3 Safety.",
	}, []string{labelNamespace, "device", "capability"})
}

type arbKey struct {
	dev        types.NamespacedName
	capability string
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
}

func ruleSource(name string) string { return rulePrefix + name }

func claimLevel(p homev1alpha1.RulePriority) control.Level {
	switch p {
	case homev1alpha1.RulePrioritySafety:
		return control.Safety
	case homev1alpha1.RulePriorityOverride:
		return control.Override
	}
	return control.Normal
}

// ControlChanged implements hub.ControlWatcher: Run looks at the device's arbiters again.
func (e *Engine) ControlChanged(dev types.NamespacedName) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.controlDue[dev] = true
	e.signal()
}

// Discovered implements driver.Sink: a device that has reported can be written.
func (e *Engine) Discovered(dev types.NamespacedName, _ driver.Info) {
	e.ControlChanged(dev)
}

// ensure creates or updates the device's arbiters from its control entries, and drops those
// whose entry has gone. Call with e.mu held.
func (e *Engine) ensure(dev types.NamespacedName) {
	snap, _ := e.Store.Get(dev)
	var saved ControlState
	loaded := false
	for _, c := range snap.Control {
		key := arbKey{dev, c.Capability}
		old := e.arbs[key]
		if old != nil && equality.Semantic.DeepEqual(old.spec, c) {
			continue
		}
		settings, err := ControlSettings(c, e.Location)
		if err != nil {
			// The operator reports it on the Device; the capability stays unarbitrated.
			e.Log.Info("Ignored invalid control entry", "device", dev, "error", err.Error())
			e.dropArb(key)
			continue
		}
		var st control.State
		if old != nil {
			st = old.a.State()
		} else {
			if !loaded {
				saved, _ = e.Controls.Get(dev)
				loaded = true
			}
			st = saved[c.Capability]
		}
		a, err := control.New(settings, e.Clock, st)
		if err != nil {
			e.Log.Error(err, "Failed to start arbiter", "device", dev, "capability", c.Capability)
			e.dropArb(key)
			continue
		}
		ar := &arb{spec: *c.DeepCopy(), a: a}
		if old != nil {
			ar.writing, ar.wrote, ar.retry = old.writing, old.wrote, old.retry
		}
		e.arbs[key] = ar
	}
	for key := range e.arbs {
		if key.dev == dev && !snap.Controls(key.capability) {
			e.dropArb(key)
		}
	}
}

// dropArb forgets an arbiter whose control entry has gone. Call with e.mu held.
func (e *Engine) dropArb(key arbKey) {
	if _, ok := e.arbs[key]; !ok {
		return
	}
	delete(e.arbs, key)
	if e.ControlLevel != nil {
		e.ControlLevel.DeleteLabelValues(key.dev.Namespace, key.dev.Name, key.capability)
	}
	e.markSave(key.dev)
	e.reportControl(key.dev)
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
	for key, ar := range e.arbs {
		if key.dev == dev {
			e.decide(ctx, key, ar)
		}
	}
	e.markSave(dev)
	e.reportControl(dev)
}

// arbiter returns the capability's arbiter, or nil when it isn't arbitrated. Call with e.mu held.
func (e *Engine) arbiter(key arbKey) *arb {
	if ar := e.arbs[key]; ar != nil {
		return ar
	}
	if snap, ok := e.Store.Get(key.dev); !ok || !snap.Controls(key.capability) {
		return nil
	}
	e.ensure(key.dev)
	return e.arbs[key]
}

// decide works out what the device should be and starts writing it. Call with e.mu held.
func (e *Engine) decide(ctx context.Context, key arbKey, ar *arb) {
	if ar.writing {
		ar.pending = true
		return
	}
	now := e.Clock.Now()
	snap, ok := e.Store.Get(key.dev)
	if v, reported := snap.Values[key.capability]; ok && reported && !ar.a.State().Known {
		// The device reported before the arbiter knew it, as on a first start.
		ar.a.Wrote(capability.Float(v))
	}
	d := ar.a.Decide()
	ar.d, ar.next = d, d.Next
	e.markSave(key.dev)
	e.reportControl(key.dev)
	if !d.Write {
		return
	}
	switch {
	case !ok || !snap.Discovered:
		// Discovered decides again.
		return
	case now.Before(ar.retry):
		if ar.next.IsZero() || ar.retry.Before(ar.next) {
			ar.next = ar.retry
		}
		return
	}
	ar.writing = true
	value := d.Value
	e.runs.Go(func() { e.writeControl(ctx, key, ar, value) })
}

// writeControl writes a decision, records it, and decides again.
func (e *Engine) writeControl(ctx context.Context, key arbKey, ar *arb, value float64) {
	err := e.Setter.Set(ctx, key.dev, key.capability, valueOf(key.capability, value))
	e.mu.Lock()
	defer e.mu.Unlock()
	ar.writing = false
	if e.arbs[key] != ar {
		return
	}
	now := e.Clock.Now()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.Log.Error(err, "Failed to write arbitrated capability", "device", key.dev, "capability", key.capability)
		ar.retry = now.Add(retryFailed)
	} else {
		ar.a.Wrote(value)
		ar.wrote, ar.retry = now, time.Time{}
	}
	ar.pending = false
	e.decide(ctx, key, ar)
	e.signal()
}

// valueOf turns an arbiter value into the capability's value.
func valueOf(name string, v float64) any {
	if spec, ok := capability.Lookup(name); ok && spec.Kind == homev1alpha1.CapabilityKindBool {
		return v != 0
	}
	return v
}

// outside records a reported value on an arbitrated capability. A value the runtime didn't
// write is an outside change. Call with e.mu held.
func (e *Engine) outside(ctx context.Context, u driver.Update) {
	key := arbKey{u.Device, u.Capability}
	ar := e.arbiter(key)
	if ar == nil {
		return
	}
	v := capability.Float(u.Value)
	st := ar.a.State()
	switch {
	case !st.Known:
		// The first value seen is the device's, not a change.
		ar.a.Wrote(v)
	case v == st.Value:
		return
	case ar.writing || e.Clock.Since(ar.wrote) < staleReport:
		e.Log.V(1).Info("Ignored report while a write settles", "device", u.Device, "capability", u.Capability, "value", u.Value)
		return
	default:
		if err := ar.a.Outside(SourceDevice, v); err != nil {
			e.Log.Error(err, "Failed to record outside change", "device", u.Device, "capability", u.Capability)
			return
		}
	}
	e.decide(ctx, key, ar)
}

// claim makes a claim on an arbitrated capability. It reports false when the capability isn't
// arbitrated, so the caller writes it as usual.
func (e *Engine) claim(ctx context.Context, dev types.NamespacedName, name string, c control.Claim) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := arbKey{dev, name}
	ar := e.arbiter(key)
	if ar == nil {
		return false
	}
	if err := ar.a.Claim(c); err != nil {
		e.Log.Error(err, "Failed to claim", "device", dev, "capability", name, "source", c.Source)
		return true
	}
	e.decide(ctx, key, ar)
	e.signal()
	return true
}

// releaseClaim ends a source's claim on an arbitrated capability. It reports false when the
// capability isn't arbitrated.
func (e *Engine) releaseClaim(ctx context.Context, dev types.NamespacedName, name, source string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := arbKey{dev, name}
	ar := e.arbiter(key)
	if ar == nil {
		return false
	}
	ar.a.Release(source)
	e.decide(ctx, key, ar)
	e.signal()
	return true
}

// releaseSource ends a source's claims on every arbitrated capability in the namespace. Call with
// e.mu held.
func (e *Engine) releaseSource(ctx context.Context, namespace, source string) {
	for key, ar := range e.arbs {
		if key.dev.Namespace != namespace || !slices.ContainsFunc(ar.a.State().Claims, func(c control.Claim) bool { return c.Source == source }) {
			continue
		}
		ar.a.Release(source)
		e.decide(ctx, key, ar)
	}
	e.signal()
}

// arbitrated reports whether a device arbitrates a capability.
func (e *Engine) arbitrated(dev types.NamespacedName, name string) bool {
	snap, ok := e.Store.Get(dev)
	return ok && snap.Controls(name)
}

// reportControl reports the device's arbitrated capabilities, if they changed at a transition.
// Call with e.mu held.
func (e *Engine) reportControl(dev types.NamespacedName) {
	var out []homev1alpha1.CapabilityControlStatus
	for key, ar := range e.arbs {
		if key.dev != dev {
			continue
		}
		out = append(out, controlStatus(key.capability, ar.d))
		if e.ControlLevel != nil {
			e.ControlLevel.WithLabelValues(dev.Namespace, dev.Name, key.capability).Set(float64(ar.d.Level))
		}
	}
	slices.SortFunc(out, func(a, b homev1alpha1.CapabilityControlStatus) int {
		return strings.Compare(a.Capability, b.Capability)
	})
	sig, _ := json.Marshal(out)
	if e.reportedControl[dev] == string(sig) {
		return
	}
	e.reportedControl[dev] = string(sig)
	if len(out) == 0 {
		delete(e.reportedControl, dev)
	}
	if e.OnControl != nil {
		e.OnControl(dev, out)
	}
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
	raw, _ := json.Marshal(valueOf(name, v))
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
// then saves what's left.
func (e *Engine) saveControls(ctx context.Context) {
	saved := map[types.NamespacedName]string{}
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
			defer cancel()
			e.saveDevices(last, saved)
			return
		case <-e.saveWake:
		}
		e.saveDevices(ctx, saved)
	}
}

// saveDevices saves the devices marked for saving, unless their state is as last saved.
func (e *Engine) saveDevices(ctx context.Context, saved map[types.NamespacedName]string) {
	e.mu.Lock()
	states := map[types.NamespacedName]ControlState{}
	for dev := range e.saveDue {
		st := ControlState{}
		for key, ar := range e.arbs {
			if key.dev == dev {
				st[key.capability] = ar.a.State()
			}
		}
		states[dev] = st
	}
	clear(e.saveDue)
	e.mu.Unlock()
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
			continue
		}
		saved[dev] = string(raw)
	}
}
