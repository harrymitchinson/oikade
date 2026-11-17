// Command home is a small home for the host's tests. It imports only the public package oikade, and
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

	"github.com/harrymitchinson/oikade"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/plugins/zigbee2mqtt"
	"github.com/harrymitchinson/oikade/sdk/plugin"
	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

// The tests bring their own devices, so this home has no devices.go: these are its handles.
func on(device string) oikade.Bool {
	return oikade.Bool{Device: device, Capability: oikade.On}
}

func open(device string) oikade.Bool {
	return oikade.Bool{Device: device, Capability: oikade.Open}
}

// humidity is the bathroom's, which the fan follows.
var humidity = oikade.Number{Device: "bathroom-sensor", Capability: oikade.Humidity}

// The design's examples in OIK-298, with the parts of them that run on the real clock left out.
var (
	poolWater  = oikade.Number{Device: "pool-water", Capability: oikade.Temperature}
	garageTemp = oikade.Number{Device: "garage-sensor", Capability: oikade.Temperature}
)

// studyHeater is a simulated heater: a Thermostat on the Matter bridge.
var studyHeater = oikade.Number{Device: "study-heater", Capability: oikade.Setpoint}

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
	return f.Handler.Set(ctx, p, contractv1.SetCommand{ID: valve, Capability: string(oikade.On), Value: true})
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

// quietRelays is a second plugin, whose devices do nothing wrong but one it can't use yet. It
// says its simulated account refuses its login until the test fixes it, and that it needs its setup
// run until it is.
func quietRelays(_ plugin.Environment, _ []contractv1.DeclaredDevice) (plugin.Handler, error) {
	locked := relay("locked")
	locked.ID += ".needs-key"
	quiet := relay("quiet")
	quiet.ID += ".needs-setup"
	h, _ := relays.New([]contractv1.DeclaredDevice{quiet, locked}).(withSetup)
	return refused{h}, nil
}

// withSetup is a plugin with a setup.
type withSetup interface {
	plugin.Handler
	plugin.Setup
}

// refusedLogin is the problem quietRelays says it has.
const refusedLogin = "its simulated account refused the login"

// refused is a plugin with a problem of its own, until the file OIKADE_TEST_PLUGIN_DOWN.fixed
// appears.
type refused struct{ withSetup }

func (r refused) Serve(ctx context.Context, p *plugin.Plugin) error {
	if err := r.withSetup.Serve(ctx, p); err != nil {
		return err
	}
	fixed := os.Getenv("OIKADE_TEST_PLUGIN_DOWN") + ".fixed"
	for {
		problem := refusedLogin
		if _, err := os.Stat(fixed); err == nil {
			problem = ""
		}
		if err := p.SetError(ctx, problem); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// routine takes every kind of step in a known order, which the tests count on: start, now, set,
// set, do, sleep, now, set, finish.
func routine(r *oikade.FlowRun) {
	began := r.Now()
	for _, lamp := range []string{"lamp-0", "lamp-1"} {
		r.Set(on(lamp).To(true))
	}
	plan, err := r.Do("plan", func(context.Context) (string, error) { return "long", call() })
	if err != nil {
		r.Fail(err)
	}
	switch plan {
	case "long":
		r.Sleep(50 * time.Millisecond)
	default:
		r.Set(on("routine-wrong").To(true))
		return
	}
	if r.Now().Before(began) {
		r.Fail(errors.New("the recorded time went backwards"))
	}
	r.Set(on("routine-done").To(true))
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
func editable(r *oikade.FlowRun) {
	wait := time.Hour
	if _, err := os.Stat(os.Getenv("OIKADE_TEST_EDITED")); err == nil {
		wait = time.Millisecond
	}
	r.Sleep(wait)
	r.Set(on("edit-out").To(true))
}

// parked waits for a sensor, with a limit that says which step it's waiting at. A deploy can land
// while it does.
func parked(r *oikade.FlowRun) {
	limit := time.Hour
	if variant == "edited" {
		limit = 2 * time.Hour
	}
	r.WaitFor(open("park-end"), limit)
	out := "park-out"
	if variant == "tail" {
		out = "park-tail"
	}
	r.Set(on(out).To(true))
}

// claiming holds a claim until a sensor says so, and releases it.
func claiming(r *oikade.FlowRun) {
	r.Set(on("claimed-flow").To(false).UntilReleased())
	limit := time.Hour
	if strings.Contains(variant, "editclaim") {
		limit = 2 * time.Hour
	}
	if !r.WaitFor(open("claim-end"), limit) {
		r.Fail(errors.New("nobody ended the claim"))
	}
	r.Release("claiming")
}

// failing holds a normal claim and a Safety one, whose decision ranks it above the override line,
// and fails when a sensor says so.
func failing(r *oikade.FlowRun) {
	r.Set(on("claimed-normal").To(false).UntilReleased())
	r.Set(on("claimed-safety").To(false).UntilReleased())
	r.WaitFor(open("fail-now"), time.Hour)
	r.Fail(errors.New("this flow fails on purpose"))
}

// setting is a flow that sets each of sets.
func setting(sets ...oikade.Set) func(*oikade.FlowRun) {
	return func(r *oikade.FlowRun) {
		for _, s := range sets {
			r.Set(s)
		}
	}
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
	case "crash-told", "crash-noflow", "crash-editclaim", "plugins-hidden-crash":
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

// fixtureSecret is the secret the tests set, declared as the home is built.
var fixtureSecret = oikade.Secret("fixture-secret")

// reportSecret tells the tests a secret arrived, and only how long it is: the value mustn't be logged.
func reportSecret() {
	if len(os.Args) < 2 || os.Args[1] != "automations" {
		return
	}
	if value, err := fixtureSecret.Read(); err == nil {
		fmt.Printf("{\"msg\":\"Read secret\",\"length\":%d}\n", len(value))
	}
}

func main() {
	start()
	reportSecret()
	home().Run()
}

// home is the home this variant builds.
func home() *oikade.Home {
	leakShutoff, claimMoment := "leak-shutoff", "claim-moment"
	switch variant {
	case "renamed":
		leakShutoff = "valve-shutoff"
	case "removed", "crash-removed", "crash-told":
		claimMoment = "claim-moment-2"
	case "badsecret":
		// A name oikade secret set refuses fails the check.
		_ = oikade.Secret("not/a-name")
	}
	plugins := strings.Contains(variant, "plugins")
	var options oikade.Options
	if variant == "cottage" {
		options.MatterName = "Cottage"
	}
	h := oikade.NewHome(options)
	// The valve under the name the home declares for it, which tests rename, or move onto the spare
	// relay, with a Safety claim on it.
	valve := valve
	switch variant {
	case "plugins-mains":
		valve = "mains-valve"
		h.Device(valve).Hardware("relays", "valve")
	case "plugins-valve":
		h.Device(valve).Hardware("relays", "valve")
	case "plugins-swapped":
		h.Device(valve).Hardware("relays", "spare-relay")
	case "plugins-landing":
		h.Device("quiet").Hardware("quiet", "quiet.needs-setup").Room("landing")
	}

	h.Flow("routine", routine).When(open("flow-go"))
	h.Flow("editable", editable).When(open("edit-go"))
	if !strings.Contains(variant, "noflow") {
		h.Flow("claiming", claiming).When(open("claim-flow-go"))
	}
	failer := h.Flow("failing", failing).When(open("fail-go"))
	if variant != "nopark" {
		h.Flow(parkedName, parked).When(open("park-go"))
	}
	h.Flow("reset", func(r *oikade.FlowRun) {
		r.Release(valve + " on " + leakShutoff)
		r.Release("siren on leak-alarm")
	}).When(open("reset"))
	h.Flow(claimMoment, setting(on("claimed").To(false).For(time.Hour))).When(open("claim-go"))
	h.Flow("study-setback", setting(studyHeater.To(18))).When(open("study-go"))
	// A time of day comes from the test, which seeds when it was last accounted for.
	if at := os.Getenv("OIKADE_TEST_AT"); at != "" {
		h.Flow("timed", setting(on("timed-out").To(true))).At(oikade.Clock(at))
	}
	if variant == "badcheck" {
		// A flow named as a case is.
		h.Flow(valve+" on "+leakShutoff, setting()).When(open("reset"))
	}
	if plugins {
		h.Flow("quiet-heard", setting(on("quiet-heard").To(true))).When(on("quiet"))
	}

	// A change here gets an answer, so a test knows when the process has caught up. It's a case,
	// which the engine works out in order, and leaves no flow run behind.
	h.Decide(on("probe-out")).Case("probe", true, open("probe")).Default(false)
	if variant != "nofan" {
		h.Decide(on("fan")).Case("fan", true, humidity.Above(70)).Default(false)
	}
	if variant != "noleak" {
		h.Decide(on(valve)).
			Case(leakShutoff, false, open("leak-sensor")).LatchUntil(open("reset")).
			OverrideFor(time.Hour).
			Flows().
			Default(true)
	}
	// With no override line, only the case's saved state remembers this latch.
	h.Decide(on("siren")).
		OverrideUntilNext().
		Case("leak-alarm", true, open("leak-sensor")).LatchUntil(open("reset")).
		Default(false)
	// Reads whichever devices are in the cellar now.
	cellar := h.Room("cellar")
	h.Decide(on("dehumidifier")).
		Case("cellar-damp", true, oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
			return slices.Max(s.Readings(cellar.Humidity)) > 65
		})).
		Default(false)
	// A bug in a home: a condition that never returns once the sensor says so.
	h.Decide(on("trap-light")).
		Case("stuck", true, oikade.ConditionFunc("trap", func(s oikade.Snapshot) bool {
			for s.Get(open("trap")) {
				continue
			}
			return false
		})).
		Default(false)
	h.Decide(on("entry-light")).
		Case("motion", true, oikade.Bool{Device: "entry-sensor", Capability: oikade.Occupied}).Linger(5 * time.Minute).
		Default(false)
	h.Decide(on("pool-pump")).
		Limits(oikade.Limits{MinimumOn: 30 * time.Minute, MinimumOff: 15 * time.Minute}).
		Case("frost", true, poolWater.Below(3)).Linger(30 * time.Minute).
		OverrideFor(2 * time.Hour).
		Default(false)
	h.Decide(on("garage-heater")).
		NoOverride().
		Case("overheat", false, garageTemp.Above(40)).LatchUntil(open("garage-door")).
		Default(true)
	// The failing flow's Safety claim: its decision ranks it above the override line.
	h.Decide(on("claimed-safety")).Flows(failer).OverrideFor(time.Hour).Flows().Default(true)
	if plugins {
		h.Decide(on(once)).OverrideFor(time.Hour).Case("once", true, open("once-sensor")).Default(false)
		// Names a plugin's device, which a test moves to another room.
		h.Decide(on("quiet-held")).Case("quiet-held", true, on("quiet")).Default(false)
	}

	// The porch's plugs are switched off at Safety on a leak, and a test declares the relay there
	// as a plug, then as a light, which the hold would no longer reach.
	if porch, ok := map[string]oikade.DeviceType{
		"plugins-porch-plug": oikade.OnOffPlugInUnit, "plugins-porch-light": oikade.OnOffLight,
	}[variant]; ok {
		h.Device("relay").Hardware("relays", "relay").Room("porch").Is(porch)
		h.Decide(h.Room("porch", oikade.Plugs).On).
			Case("porch-leak", false, open("leak-sensor")).
			OverrideFor(time.Hour).
			Default(true)
	}
	if variant == "plugins-declared" {
		h.Device("porch-relay").Hardware("relays", "relay").Room("Porch").Is(oikade.OnOffLight)
		h.Device("shed-fan").Room("Shed")
	}
	switch {
	case plugins:
		imported := h.Plugin("relays", faultyRelays)
		if strings.Contains(variant, "hidden") {
			// As a platform that puts its devices in the Home app itself, but one.
			imported.HideFromMatter().ShowOnMatter(valve)
		}
		h.Plugin("quiet", quietRelays)
	case variant == "zigbee":
		h.Plugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{
			// A test's broker has a port of its own. A home writes its broker here.
			Broker: os.Getenv("OIKADE_TEST_ZIGBEE_BROKER"),
		}))
	}
	return h
}

// parkedName is the flow that waits, which some variants drop.
const parkedName = "parked"
