// Command home is a small home for the host's tests. It imports only the public framework, and
// the tests build it as a module of its own to prove that's enough.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/plugins/zigbee2mqtt"
	"github.com/harrymitchinson/oikade/sdk/plugin"
	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

const (
	on   = "switch.on"
	open = "sensor.contact-open"
)

// The devices a test has plugins do something with.
const (
	valve  = "valve"
	panics = "panics"
	hangs  = "hangs"
	once   = "once"
)

// variant makes this home a different version, set when the tests link it. Empty is the first one.
var variant string

// wired are the relays a home with plugins configures in Go, which are all there are the first time.
var wired = []contractv1.DeclaredDevice{relay("relay"), relay(valve), relay(panics), relay(hangs), relay(once)}

// relay is a device called id.
func relay(id string) contractv1.DeclaredDevice {
	return contractv1.DeclaredDevice{ID: id, Name: id, DisplayName: id}
}

// faulty is a plugin that goes wrong on request, and whose valve always starts open.
type faulty struct{ plugin.Handler }

func (f faulty) Serve(ctx context.Context, p *plugin.Plugin) error {
	if err := f.Handler.Serve(ctx, p); err != nil {
		return err
	}
	return f.Handler.Set(ctx, p, contractv1.SetCommand{ID: valve, Capability: on, Value: true})
}

func (f faulty) Set(ctx context.Context, p *plugin.Plugin, cmd contractv1.SetCommand) error {
	switch cmd.ID {
	case panics:
		panic("this plugin was told to panic")
	case hangs:
		select {}
	case once:
		mark := os.Getenv("OIKADE_TEST_PLUGIN_DOWN") + ".once"
		if _, err := os.Stat(mark); err != nil && cmd.Value == true {
			_ = os.WriteFile(mark, nil, 0o600)
			panic("this plugin took a write with it")
		}
	}
	return f.Handler.Set(ctx, p, cmd)
}

// faultyRelays builds the plugin, unless the test has it staying down.
func faultyRelays(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
	if _, err := os.Stat(os.Getenv("OIKADE_TEST_PLUGIN_DOWN")); err == nil {
		return nil, errors.New("this plugin was told to stay down")
	}
	for _, d := range wired {
		if !slices.ContainsFunc(devices, func(k contractv1.DeclaredDevice) bool { return k.ID == d.ID }) {
			devices = append(devices, d)
		}
	}
	return faulty{relays.New(devices)}, nil
}

// quietRelays is a second plugin, which does nothing wrong.
func quietRelays(_ plugin.Environment, _ []contractv1.DeclaredDevice) (plugin.Handler, error) {
	return relays.New([]contractv1.DeclaredDevice{relay("quiet")}), nil
}

// routine takes every kind of step in a known order, which the tests count on: start, now, set,
// set, do, sleep, now, set, finish.
func routine(r *framework.FlowRun) error {
	began := r.Now()
	for _, lamp := range []string{"lamp-0", "lamp-1"} {
		if err := r.Set(framework.Set{Device: lamp, Capability: on, Value: true}); err != nil {
			return err
		}
	}
	var plan string
	if err := r.Do("plan", &plan, func(context.Context) error {
		plan = "long"
		return call()
	}); err != nil {
		return err
	}
	switch plan {
	case "long":
		r.Sleep(50 * time.Millisecond)
	default:
		return r.Set(framework.Set{Device: "routine-wrong", Capability: on, Value: true})
	}
	if r.Now().Before(began) {
		return errors.New("the recorded time went backwards")
	}
	return r.Set(framework.Set{Device: "routine-done", Capability: on, Value: true})
}

// call is an outside call the tests can count, as a line in a file.
func call() error {
	f, err := os.OpenFile(os.Getenv("OIKADE_TEST_CALLS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintln(f, "plan")
	return err
}

// editable waits for an hour, unless the test has "edited" it.
func editable(r *framework.FlowRun) error {
	wait := time.Hour
	if _, err := os.Stat(os.Getenv("OIKADE_TEST_EDITED")); err == nil {
		wait = time.Millisecond
	}
	r.Sleep(wait)
	return r.Set(framework.Set{Device: "edit-out", Capability: on, Value: true})
}

// parked waits for a sensor, with a limit that says which step it's waiting at. A deploy can land
// while it does.
func parked(r *framework.FlowRun) error {
	limit := time.Hour
	if variant == "edited" {
		limit = 2 * time.Hour
	}
	r.WaitFor(func(s framework.State) bool { return s.Bool("park-end", open) }, limit)
	out := "park-out"
	if variant == "tail" {
		out = "park-tail"
	}
	return r.Set(framework.Set{Device: out, Capability: on, Value: true})
}

// claiming holds a claim until a sensor says so, and releases it.
func claiming(r *framework.FlowRun) error {
	err := r.Set(framework.Set{Device: "claimed-flow", Capability: on, Value: false, UntilReleased: true})
	if err != nil {
		return err
	}
	if !r.WaitFor(func(s framework.State) bool { return s.Bool("claim-end", open) }, time.Hour) {
		return errors.New("nobody ended the claim")
	}
	return r.Release("claiming")
}

// failing holds a normal claim and a Safety one, and fails when a sensor says so.
func failing(r *framework.FlowRun) error {
	err := r.Set(framework.Set{Device: "claimed-normal", Capability: on, Value: false, UntilReleased: true})
	if err != nil {
		return err
	}
	safety := framework.Set{Device: "claimed-safety", Capability: on, Value: false, UntilReleased: true}
	safety.Priority = framework.Safety
	if err = r.Set(safety); err != nil {
		return err
	}
	r.WaitFor(func(s framework.State) bool { return s.Bool("fail-now", open) }, time.Hour)
	return errors.New("this flow fails on purpose")
}

// start acts out the variants that fail at run time. Check mode doesn't reach this far.
func start() {
	if len(os.Args) < 2 || os.Args[1] != "automations" {
		return
	}
	switch variant {
	case "crash":
		os.Exit(3)
	case "crash-late", "crash-removed":
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(3)
		}()
	case "crash-told", "crash-noflow":
		go func() {
			for {
				if _, err := os.Stat(os.Getenv("OIKADE_TEST_CRASH")); err == nil {
					os.Exit(3)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
}

func main() {
	start()
	var extra []framework.Moment
	// A time of day comes from the test, which seeds when it was last accounted for.
	if at := os.Getenv("OIKADE_TEST_AT"); at != "" {
		extra = append(extra, framework.Moment{
			Name: "timed", At: []string{at},
			Do: []framework.Action{framework.Set{Device: "timed-out", Capability: on, Value: true}},
		})
	}
	leakShutoff, claimMoment := "leak-shutoff", framework.Moment{
		Name: "claim-moment", On: []framework.Change{{Device: "claim-go", Capability: open}},
		Do: []framework.Action{framework.Set{Device: "claimed", Capability: on, Value: false, For: time.Hour}},
	}
	switch variant {
	case "renamed":
		leakShutoff = "valve-shutoff"
	case "removed", "crash-removed", "crash-told":
		claimMoment.Name = "claim-moment-2"
	case "badcheck":
		extra = append(extra, framework.Moment{Name: leakShutoff})
	}
	run(framework.Home{
		Holds: []framework.Hold{
			{
				Name: "fan", Device: "fan", Capability: on, Value: true,
				When: func(s framework.State) bool {
					return s.Number("bathroom-sensor", "sensor.relative-humidity") > 70
				},
			},
			{
				Name: leakShutoff, Device: valve, Capability: on, Value: false,
				Priority: framework.Safety, Latch: true,
				When: func(s framework.State) bool {
					return s.Bool("leak-sensor", open)
				},
			},
			{
				// With no arbiter to hold a claim, only the hold's saved state remembers this latch.
				Name: "leak-alarm", Device: "siren", Capability: on, Value: true, Latch: true,
				When: func(s framework.State) bool {
					return s.Bool("leak-sensor", open)
				},
			},
			{
				// A bug in a home: a condition that never returns once the sensor says so.
				Name: "stuck", Device: "trap-light", Capability: on, Value: true,
				When: func(s framework.State) bool {
					for s.Bool("trap", open) {
						continue
					}
					return false
				},
			},
		},
		Flows: []framework.Flow{
			{Name: "routine", On: []framework.Change{{Device: "flow-go", Capability: open}}, Run: routine},
			{Name: "editable", On: []framework.Change{{Device: "edit-go", Capability: open}}, Run: editable},
			{Name: "claiming", On: []framework.Change{{Device: "claim-flow-go", Capability: open}}, Run: claiming},
			{Name: "failing", On: []framework.Change{{Device: "fail-go", Capability: open}}, Run: failing},
			{Name: parkedName, On: []framework.Change{{Device: "park-go", Capability: open}}, Run: parked},
		},
		Moments: append([]framework.Moment{
			{
				// A change here gets an answer, so a test knows when the process has caught up.
				Name: "probe", On: []framework.Change{{Device: "probe", Capability: open}}, Restart: true,
				Do: []framework.Action{framework.Set{Device: "probe-out", Capability: on, Value: true}},
			},
			{
				Name: "reset", On: []framework.Change{{Device: "reset", Capability: open}},
				Do: []framework.Action{framework.Release(leakShutoff), framework.Release("leak-alarm")},
			},
			claimMoment,
		}, extra...),
	})
}

// parkedName is the flow that waits, which some variants drop.
const parkedName = "parked"

// run leaves out what the variants without a rule leave out.
func run(h framework.Home) {
	if strings.Contains(variant, "noflow") {
		h.Flows = slices.DeleteFunc(h.Flows, func(f framework.Flow) bool { return f.Name == "claiming" })
	}
	if variant == "nofail" {
		h.Flows = slices.DeleteFunc(h.Flows, func(f framework.Flow) bool { return f.Name == "failing" })
	}
	if variant == "parkmoment" {
		h.Flows = slices.DeleteFunc(h.Flows, func(f framework.Flow) bool { return f.Name == parkedName })
		h.Moments = append(h.Moments, framework.Moment{
			Name: parkedName, On: []framework.Change{{Device: "park-go", Capability: open}},
			Do: []framework.Action{framework.Set{Device: "park-out", Capability: on, Value: true}},
		})
	}
	if variant == "nofan" {
		h.Holds = h.Holds[1:] // The fan comes first.
	}
	if variant == "nopark" {
		h.Flows = slices.DeleteFunc(h.Flows, func(f framework.Flow) bool { return f.Name == parkedName })
	}
	if variant == "noleak" {
		h.Holds = slices.DeleteFunc(h.Holds, func(r framework.Hold) bool { return r.Name == "leak-shutoff" })
	}
	if strings.Contains(variant, "plugins") {
		h.Plugins = []framework.Plugin{{Name: "relays", New: faultyRelays}, {Name: "quiet", New: quietRelays}}
		h.Holds = append(h.Holds, framework.Hold{
			Name: "once", Device: once, Capability: on, Value: true,
			When: func(s framework.State) bool { return s.Bool("once-sensor", open) },
		})
	}
	if variant == "zigbee" {
		h.Plugins = []framework.Plugin{{Name: "zigbee", New: zigbee2mqtt.New(zigbee2mqtt.Config{})}}
	}
	framework.Run(h)
}
