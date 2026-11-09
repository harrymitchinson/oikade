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
	limit := time.Hour
	if strings.Contains(variant, "editclaim") {
		limit = 2 * time.Hour
	}
	if !r.WaitFor(func(s framework.State) bool { return s.Get(open("claim-end")) }, limit) {
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
	case "crash-told", "crash-noflow", "crash-editclaim":
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
	framework.Run(home().home())
}

// when is the rule, or nothing if a variant leaves it out.
func when[T framework.Part](keep bool, rule T) []T {
	if keep {
		return []T{rule}
	}
	return nil
}

// home is the home this variant builds.
func home() rules {
	leakShutoff, claimMoment := "leak-shutoff", "claim-moment"
	switch variant {
	case "renamed":
		leakShutoff = "valve-shutoff"
	case "removed", "crash-removed", "crash-told":
		claimMoment = "claim-moment-2"
	}
	plugins := strings.Contains(variant, "plugins")

	holds := slices.Concat(
		when(variant != "nofan", framework.NewHold("fan", on("fan").To(true)).
			When(func(s framework.State) bool {
				return s.Get(humidity) > 70
			})),
		when(variant != "noleak", framework.NewHold(leakShutoff, on(valve).To(false)).
			Priority(framework.Safety).
			Latch().
			When(func(s framework.State) bool {
				return s.Get(open("leak-sensor"))
			})),
		[]framework.Hold{
			// With no arbiter to hold a claim, only the hold's saved state remembers this latch.
			framework.NewHold("leak-alarm", on("siren").To(true)).
				Latch().
				When(func(s framework.State) bool {
					return s.Get(open("leak-sensor"))
				}),
			// Reads whichever devices are in the cellar now, which oikade devices room changes.
			framework.NewHold("cellar-damp", on("dehumidifier").To(true)).
				When(func(s framework.State) bool {
					return slices.Max(s.Readings(framework.InRoom("cellar", framework.Humidity))) > 65
				}),
			// A bug in a home: a condition that never returns once the sensor says so.
			framework.NewHold("stuck", on("trap-light").To(true)).
				When(func(s framework.State) bool {
					for s.Get(open("trap")) {
						continue
					}
					return false
				}),
		},
		when(plugins, framework.NewHold("once", on(once).To(true)).
			When(func(s framework.State) bool { return s.Get(open("once-sensor")) })),
		// Names a plugin's device, which a test moves to another room.
		when(plugins, framework.NewHold("quiet-held", on("quiet-held").To(true)).
			When(func(s framework.State) bool { return s.Get(on("quiet")) })),
		// A Safety rule that keeps the porch's plugs on, and only its plugs.
		when(variant == "typed", framework.NewHold("porch-plugs",
			framework.InRoom("porch", framework.On).Types(framework.OnOffPlugInUnit).To(true)).
			Priority(framework.Safety).
			When(func(s framework.State) bool { return s.Get(open("porch-go")) })),
	)

	// parkmoment turns the parked flow into a moment of the same name.
	parkedStays := variant != "parkmoment" && variant != "nopark"
	flows := slices.Concat(
		[]framework.Flow{
			framework.NewFlow("routine", routine).On(open("flow-go")),
			framework.NewFlow("editable", editable).On(open("edit-go")),
		},
		when(!strings.Contains(variant, "noflow"), framework.NewFlow("claiming", claiming).On(open("claim-flow-go"))),
		when(variant != "nofail", framework.NewFlow("failing", failing).On(open("fail-go"))),
		when(parkedStays, framework.NewFlow(parkedName, parked).On(open("park-go"))),
	)

	moments := slices.Concat(
		[]framework.Moment{
			// A change here gets an answer, so a test knows when the process has caught up.
			framework.NewMoment("probe", on("probe-out").To(true)).On(open("probe")).Restart(),
			framework.NewMoment("reset", framework.Release(leakShutoff), framework.Release("leak-alarm")).On(open("reset")),
			framework.NewMoment(claimMoment, on("claimed").To(false).For(time.Hour)).On(open("claim-go")),
		},
		// A time of day comes from the test, which seeds when it was last accounted for.
		when(os.Getenv("OIKADE_TEST_AT") != "",
			framework.NewMoment("timed", on("timed-out").To(true)).At(framework.Clock(os.Getenv("OIKADE_TEST_AT")))),
		when(variant == "badcheck", framework.NewMoment(leakShutoff)),
		when(variant == "parkmoment", framework.NewMoment(parkedName, on("park-out").To(true)).On(open("park-go"))),
		when(plugins, framework.NewMoment("quiet-heard", on("quiet-heard").To(true)).On(on("quiet"))),
	)

	var pluginParts []framework.Plugin
	var control []framework.Control
	switch {
	case plugins:
		pluginParts = []framework.Plugin{
			framework.NewPlugin("relays", faultyRelays), framework.NewPlugin("quiet", quietRelays),
		}
		control = []framework.Control{
			framework.NewControl(on(valve).To(true), framework.PreferOff, framework.ExternalOverride).OverrideHold(time.Hour),
			framework.NewControl(on(once).To(false), framework.PreferOn, framework.ExternalOverride).OverrideHold(time.Hour),
		}
	case variant == "zigbee":
		pluginParts = []framework.Plugin{framework.NewPlugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{}))}
	}
	return rules{Holds: holds, Moments: moments, Flows: flows, Control: control, Plugins: pluginParts}
}

// parkedName is the flow that waits, which some variants drop.
const parkedName = "parked"

// rules is a home by kind.
type rules struct {
	Holds   []framework.Hold
	Moments []framework.Moment
	Flows   []framework.Flow
	Control []framework.Control
	Plugins []framework.Plugin
}

func (r rules) home() framework.Home {
	return framework.NewHome(framework.Options{}, bundle(r.Holds), bundle(r.Moments), bundle(r.Flows), bundle(r.Control),
		bundle(r.Plugins))
}

func bundle[T framework.Part](parts []T) framework.Bundle {
	all := make([]framework.Part, 0, len(parts))
	for _, p := range parts {
		all = append(all, p)
	}
	return framework.NewBundle(all...)
}
