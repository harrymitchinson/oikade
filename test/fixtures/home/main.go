// Command home is a small home for the host's tests. It imports only the public package oikade, and
// the tests build it as a module of its own to prove that's enough.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"oikade.dev"
	"oikade.dev/contract/v1"
	"oikade.dev/plugins/zigbee2mqtt"
	"oikade.dev/sdk/plugin"
	"oikade.dev/test/fixtures/simswitch/relays"
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

// bedroomAC is a simulated air conditioner's mode, which the host keeps as a number and a home
// writes as a name.
var bedroomAC = oikade.Choice{Device: "bedroom-ac", Capability: oikade.Mode}

// The devices a test has plugins do something with.
const (
	valve  = "valve"
	panics = "panics"
	hangs  = "hangs"
	once   = "once"
)

// hardware is a plugin's device as a handle has it, its hardware, as oikade types writes it.
func hardware(name, id string) string { return name + "/" + id }

// variant makes this home a different version, set when the tests link it. Empty is the first one.
var variant string

// wired are the relays a home with plugins configures in Go, which are all there are the first time.
var wired = []contract.DeclaredDevice{relay("relay"), relay(valve), relay(panics), relay(hangs), relay(once)}

func relay(id string) contract.DeclaredDevice {
	return contract.DeclaredDevice{ID: id, Name: id, DisplayName: id}
}

// faulty is a plugin that goes wrong on request, and whose valve always starts open.
type faulty struct{ plugin.Handler }

func (f faulty) Serve(ctx context.Context, p *plugin.Plugin) error {
	if err := f.Handler.Serve(ctx, p); err != nil {
		return err
	}
	return f.Handler.Set(ctx, p, contract.SetCommand{ID: valve, Capability: string(oikade.On), Value: true})
}

func (f faulty) Set(ctx context.Context, p *plugin.Plugin, cmd contract.SetCommand) error {
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
func faultyRelays(_ plugin.Environment, devices []contract.DeclaredDevice) (plugin.Handler, error) {
	if _, err := os.Stat(os.Getenv("OIKADE_TEST_PLUGIN_DOWN")); err == nil {
		return nil, errors.New("this plugin was told to stay down")
	}
	for _, d := range wired {
		if !slices.ContainsFunc(devices, func(k contract.DeclaredDevice) bool { return k.ID == d.ID }) {
			devices = append(devices, d)
		}
	}
	return faulty{relays.New(devices)}, nil
}

// quietRelays is a second plugin, whose devices do nothing wrong but one it can't use yet. It
// says its simulated account refuses its login until the test fixes it, and that it needs its setup
// run until it is. It says what its setup kept as it starts, by a digest, as the plugin reads it.
func quietRelays(env plugin.Environment, _ []contract.DeclaredDevice) (plugin.Handler, error) {
	if kept, ok, err := plugin.ReadSecret(env, relays.PairingSecret); err == nil && ok {
		sum := sha256.Sum256([]byte(kept))
		fmt.Printf("{\"msg\":\"Read kept secret\",\"variant\":%q,\"sha\":%q}\n", variant, hex.EncodeToString(sum[:8]))
	}
	locked := relay("locked")
	locked.ID += ".needs-key"
	quiet := relay("quiet")
	quiet.ID += ".needs-setup"
	h, _ := relays.New([]contract.DeclaredDevice{quiet, locked}).(withSetup)
	return refused{h}, nil
}

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

// automations says whether this is the automations process, rather than check mode or a plugin.
func automations() bool { return len(os.Args) > 1 && os.Args[1] == "automations" }

// start acts out the variants that fail at run time. Check mode doesn't reach this far.
func start() {
	if !automations() {
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

// The values the tests deploy, which the variants "vars" and "vars-more" use.
const (
	fixtureToken = "FIXTURE_TOKEN"
	fixtureExtra = "FIXTURE_EXTRA"
)

// reportVar tells the tests which value a process was given, by a digest of it: the value mustn't
// be logged.
func reportVar(role, name, value string) {
	sum := sha256.Sum256([]byte(value))
	fmt.Printf("{\"msg\":\"Read var\",\"role\":%q,\"variant\":%q,\"name\":%q,\"sha\":%q}\n",
		role, variant, name, hex.EncodeToString(sum[:8]))
}

// keyedRelays is a plugin whose config, as a home writes one, has a value from oikade.Var. It says
// which it was given as it starts.
func keyedRelays(token string) plugin.Factory {
	return func(plugin.Environment, []contract.DeclaredDevice) (plugin.Handler, error) {
		reportVar("plugin", fixtureToken, token)
		return relays.New([]contract.DeclaredDevice{relay("keyed")}), nil
	}
}

func main() {
	start()
	home().Run()
}

// declareByHardware declares the variant's devices with oikade.Device.
func declareByHardware(h *oikade.Home) {
	switch variant {
	case "plugins-warm-valve":
		// A relay reports no temperature, so a deploy refuses the line.
		oikade.Device[struct {
			oikade.OnOffPlugInUnit
			Temperature oikade.Number
		}](hardware("relays", valve))
	case "cuisine":
		cuisine := oikade.Room("cuisine")
		oikade.Device[oikade.OccupancySensor]("cuisine-motion", cuisine)
		light := oikade.Device[oikade.OnOffLight]("cuisine-light", cuisine, oikade.Label("Plafond"))
		h.Decide(light.On).Case("someone-in", true, cuisine.Occupied.Any()).Default(false)
	}
}

// home is the home this variant builds.
func home() *oikade.Home {
	leakShutoff, claimMoment := "leak-shutoff", "claim-moment"
	switch variant {
	case "renamed":
		leakShutoff = "valve-shutoff"
	case "removed", "crash-removed", "crash-told":
		claimMoment = "claim-moment-2"
	}
	plugins := strings.Contains(variant, "plugins")
	var options oikade.Options
	if variant == "cottage" {
		options.MatterName = "Cottage"
	}
	h := oikade.NewHome(options)
	// The valve under the name the home declares for it, which tests rename, or move onto the spare
	// relay, with a Safety claim on it. Its handle has its hardware, which a rename leaves as it is.
	valve, valveHandle := valve, valve
	if plugins {
		valveHandle = hardware("relays", valve)
	}
	switch variant {
	case "plugins-mains":
		valve = "mains-valve"
		h.Device(valve).Hardware("relays", "valve")
	case "plugins-valve":
		h.Device(valve).Hardware("relays", "valve")
	case "plugins-swapped":
		h.Device(valve).Hardware("relays", "spare-relay")
		valveHandle = hardware("relays", "spare-relay")
	case "plugins-landing":
		h.Device("quiet").Hardware("quiet", "quiet.needs-setup").Room("landing")
	}
	quiet := hardware("quiet", "quiet.needs-setup")

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
	h.Flow("bedroom-dry-out", setting(bedroomAC.To(oikade.ModeDry).For(time.Hour))).When(open("bedroom-damp"))
	// A time of day comes from the test, which seeds when it was last accounted for.
	if at := os.Getenv("OIKADE_TEST_AT"); at != "" {
		h.Flow("timed", setting(on("timed-out").To(true))).At(oikade.Clock(at))
	}
	if variant == "badcheck" {
		// A flow named as a case is.
		h.Flow(valve+" on "+leakShutoff, setting()).When(open("reset"))
	}
	if plugins {
		h.Flow("quiet-heard", setting(on("quiet-heard").To(true))).When(on(quiet))
	}

	// A change here gets an answer, so a test knows when the process has caught up. It's a case,
	// which the engine works out in order, and leaves no flow run behind.
	h.Decide(on("probe-out")).Case("probe", true, open("probe")).Default(false)
	if variant != "nofan" {
		h.Decide(on("fan")).Case("fan", true, humidity.Above(70)).Default(false)
	}
	if variant != "noleak" {
		h.Decide(on(valveHandle)).
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
	cellar := h.Room(oikade.Room("cellar"))
	h.Decide(on("dehumidifier")).
		Case("cellar-damp", true, oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
			return slices.Max(s.Readings(cellar.Humidity)) > 65
		})).
		Default(false)
	// A bug in a home: a condition that never returns once the sensor says so. It marks the file
	// OIKADE_TEST_TRAPPED, if set, as it starts to spin.
	h.Decide(on("trap-light")).
		Case("stuck", true, oikade.ConditionFunc("trap", func(s oikade.Snapshot) bool {
			if s.Get(open("trap")) {
				if mark := os.Getenv("OIKADE_TEST_TRAPPED"); mark != "" {
					_ = os.WriteFile(mark, nil, 0o600)
				}
			}
			for s.Get(open("trap")) {
				continue
			}
			return false
		})).
		Default(false)
	h.Decide(on("entry-light")).
		Case("motion", true, oikade.Bool{Device: "entry-sensor", Capability: oikade.Occupied}).Linger(5 * time.Minute).
		Default(false)
	// Whether anyone's home, which a variant renames.
	signalName := "anyone-home"
	if variant == "renamed-signal" {
		signalName = "someone-home"
	}
	if variant != "nosignals" {
		presence := oikade.Bool{Device: "presence-sensor", Capability: oikade.Occupied}
		anyoneHome := h.Signal(signalName, presence).Linger(time.Hour)
		h.Decide(on("away-mode")).Case("away", true, oikade.Not(anyoneHome)).Default(false)
		// One that changes as the sensor does, for its history.
		h.Signal("seen-now", presence)
	}
	h.Decide(on("pool-pump")).
		Limits(oikade.Limits{MinimumOn: 30 * time.Minute, MinimumOff: 15 * time.Minute}).
		Case("frost", true, poolWater.Below(3)).Linger(30 * time.Minute).
		OverrideFor(2 * time.Hour).
		Default(false)
	h.Decide(on("garage-heater")).
		NoOverride().
		Case("overheat", false, garageTemp.Above(40)).LatchUntil(open("garage-door")).
		Default(true)
	h.Decide(bedroomAC).Flows().Default(oikade.ModeCool)
	// The failing flow's Safety claim: its decision ranks it above the override line.
	h.Decide(on("claimed-safety")).Flows(failer).OverrideFor(time.Hour).Flows().Default(true)
	if plugins {
		h.Decide(on(hardware("relays", once))).OverrideFor(time.Hour).Case("once", true, open("once-sensor")).Default(false)
		// Names a plugin's device, which a test moves to another room.
		h.Decide(on("quiet-held")).Case("quiet-held", true, on(quiet)).Default(false)
	}

	// The porch's plugs are switched off at Safety on a leak, and a test declares the relay there
	// as a plug, then as a light, which the hold would no longer reach.
	if porch, ok := map[string]oikade.DeviceType{
		"plugins-porch-plug": oikade.OnOffPlugInUnit{}, "plugins-porch-light": oikade.OnOffLight{},
	}[variant]; ok {
		h.Device("relay").Hardware("relays", "relay").Room("porch").Is(porch)
		h.Decide(h.Room(oikade.Room("porch"), oikade.Plugs).On).
			Case("porch-leak", false, open("leak-sensor")).
			OverrideFor(time.Hour).
			Default(true)
	}
	if variant == "plugins-declared" {
		h.Device("porch-relay").Hardware("relays", "relay").Room("porch").Is(oikade.OnOffLight{})
		h.Device("shed-fan").Room("shed")
	}
	declareByHardware(h)
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
	case strings.HasPrefix(variant, "vars"):
		token := oikade.Var(fixtureToken)
		if variant == "vars-more" {
			_ = oikade.Var(fixtureExtra)
		}
		if automations() {
			reportVar("automations", fixtureToken, token)
		}
		h.Plugin("keyed", keyedRelays(token))
	}
	return h
}

// parkedName is the flow that waits, which some variants drop.
const parkedName = "parked"
