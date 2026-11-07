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

// The tests bring their own devices, so this home has no devices.go: these are its handles.
func on(device string) framework.Handle[bool] {
	return framework.Handle[bool]{Device: device, Capability: framework.On}
}

func open(device string) framework.Handle[bool] {
	return framework.Handle[bool]{Device: device, Capability: framework.Open}
}

// humidity is the bathroom's, which the fan follows.
var humidity = framework.Handle[float64]{Device: "bathroom-sensor", Capability: framework.Humidity}

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
	return f.Handler.Set(ctx, p, contractv1.SetCommand{ID: valve, Capability: string(framework.On), Value: true})
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
		if err := r.Set(on(lamp).To(true)); err != nil {
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
		return r.Set(on("routine-wrong").To(true))
	}
	if r.Now().Before(began) {
		return errors.New("the recorded time went backwards")
	}
	return r.Set(on("routine-done").To(true))
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
	return r.Set(on("edit-out").To(true))
}

// parked waits for a sensor, with a limit that says which step it's waiting at. A deploy can land
// while it does.
func parked(r *framework.FlowRun) error {
	limit := time.Hour
	if variant == "edited" {
		limit = 2 * time.Hour
	}
	r.WaitFor(func(s framework.State) bool { return s.Get(open("park-end")) }, limit)
	out := "park-out"
	if variant == "tail" {
		out = "park-tail"
	}
	return r.Set(on(out).To(true))
}

// claiming holds a claim until a sensor says so, and releases it.
func claiming(r *framework.FlowRun) error {
	err := r.Set(on("claimed-flow").To(false).UntilReleased())
	if err != nil {
		return err
	}
	if !r.WaitFor(func(s framework.State) bool { return s.Get(open("claim-end")) }, time.Hour) {
		return errors.New("nobody ended the claim")
	}
	return r.Release("claiming")
}

// failing holds a normal claim and a Safety one, and fails when a sensor says so.
func failing(r *framework.FlowRun) error {
	err := r.Set(on("claimed-normal").To(false).UntilReleased())
	if err != nil {
		return err
	}
	if err = r.Set(on("claimed-safety").To(false).UntilReleased().Priority(framework.Safety)); err != nil {
		return err
	}
	r.WaitFor(func(s framework.State) bool { return s.Get(open("fail-now")) }, time.Hour)
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

// reportSecret tells the tests a secret arrived, and only how long it is: the value mustn't be logged.
func reportSecret() {
	if len(os.Args) < 2 || os.Args[1] != "automations" {
		return
	}
	if value, ok, err := framework.Secret("fixture-secret"); ok && err == nil {
		fmt.Printf("{\"msg\":\"Read secret\",\"length\":%d}\n", len(value))
	}
}

func main() {
	start()
	reportSecret()
	var extra []framework.Moment
	// A time of day comes from the test, which seeds when it was last accounted for.
	if at := os.Getenv("OIKADE_TEST_AT"); at != "" {
		extra = append(extra, framework.Moment{
			Name: "timed", At: []framework.Time{framework.Clock(at)},
			Do: []framework.Action{on("timed-out").To(true)},
		})
	}
	leakShutoff, claimMoment := "leak-shutoff", framework.Moment{
		Name: "claim-moment", On: []framework.Change{open("claim-go")},
		Do: []framework.Action{on("claimed").To(false).For(time.Hour)},
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
				Name: "fan", Set: on("fan").To(true),
				When: func(s framework.State) bool {
					return s.Get(humidity) > 70
				},
			},
			{
				Name: leakShutoff, Set: on(valve).To(false),
				Priority: framework.Safety, Latch: true,
				When: func(s framework.State) bool {
					return s.Get(open("leak-sensor"))
				},
			},
			{
				// With no arbiter to hold a claim, only the hold's saved state remembers this latch.
				Name: "leak-alarm", Set: on("siren").To(true), Latch: true,
				When: func(s framework.State) bool {
					return s.Get(open("leak-sensor"))
				},
			},
			{
				// A bug in a home: a condition that never returns once the sensor says so.
				Name: "stuck", Set: on("trap-light").To(true),
				When: func(s framework.State) bool {
					for s.Get(open("trap")) {
						continue
					}
					return false
				},
			},
		},
		Flows: []framework.Flow{
			{Name: "routine", On: []framework.Change{open("flow-go")}, Run: routine},
			{Name: "editable", On: []framework.Change{open("edit-go")}, Run: editable},
			{Name: "claiming", On: []framework.Change{open("claim-flow-go")}, Run: claiming},
			{Name: "failing", On: []framework.Change{open("fail-go")}, Run: failing},
			{Name: parkedName, On: []framework.Change{open("park-go")}, Run: parked},
		},
		Moments: append([]framework.Moment{
			{
				// A change here gets an answer, so a test knows when the process has caught up.
				Name: "probe", On: []framework.Change{open("probe")}, Restart: true,
				Do: []framework.Action{on("probe-out").To(true)},
			},
			{
				Name: "reset", On: []framework.Change{open("reset")},
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
			Name: parkedName, On: []framework.Change{open("park-go")},
			Do: []framework.Action{on("park-out").To(true)},
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
		h.Control = []framework.Control{
			{
				Default: on(valve).To(true), Resolve: framework.PreferOff,
				External: framework.ExternalOverride, OverrideHold: time.Hour,
			},
			{
				Default: on(once).To(false), Resolve: framework.PreferOn,
				External: framework.ExternalOverride, OverrideHold: time.Hour,
			},
		}
		h.Holds = append(h.Holds, framework.Hold{
			Name: "once", Set: on(once).To(true),
			When: func(s framework.State) bool { return s.Get(open("once-sensor")) },
		})
	}
	if variant == "typed" {
		// A Safety rule that keeps the porch's plugs on, and only its plugs.
		h.Holds = append(h.Holds, framework.Hold{
			Name: "porch-plugs", Priority: framework.Safety,
			Set:  framework.InRoom("porch", framework.On).Types(framework.OnOffPlugInUnit).To(true),
			When: func(s framework.State) bool { return s.Get(open("porch-go")) },
		})
	}
	if variant == "zigbee" {
		h.Plugins = []framework.Plugin{{Name: "zigbee", New: zigbee2mqtt.New(zigbee2mqtt.Config{})}}
	}
	framework.Run(h)
}
