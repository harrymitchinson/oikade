package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// aHealthyHome is a home with nothing wrong: deployed, its automations connected, its bridge serving
// a controller, and its devices online.
func aHealthyHome() *api.Status {
	on := true
	return &api.Status{
		Home: api.Home{Version: "v3", Process: true},
		Devices: []api.Device{
			{Name: "boiler", Ref: "device/boiler", Room: "utility", Online: &on, Plugin: "opentherm",
				Capabilities: []api.Capability{{Name: "on", Value: true}}},
			{Name: "hall-lamp", Ref: "device/hall-lamp", Room: "hall", Online: &on,
				Capabilities: []api.Capability{{Name: "on", Value: false}}},
		},
		Rules:   []api.Rule{{Name: "morning", Ref: "flow/morning", Kind: "flow"}},
		Plugins: []api.Plugin{{Name: "opentherm", Ref: "plugin/opentherm", Devices: []string{"boiler"}}},
		Matter:  &api.Matter{Serving: true, Fabrics: []api.Fabric{{Index: 1}}},
	}
}

// --check fails for what a command fixes, and for a device that's only offline only when it or its
// room is named.
func TestCheckFailsForWhatACommandFixes(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name   string
		break_ func(*api.Status)
		refs   []string
		fails  bool
	}{
		{"nothing wrong", func(*api.Status) {}, nil, false},
		{"no home deployed", func(st *api.Status) { st.Home = api.Home{} }, nil, true},
		{"automations down", func(st *api.Status) { st.Home.Process = false }, nil, true},
		{"the bridge down", func(st *api.Status) { st.Matter.Serving = false }, nil, true},
		{"a plugin's problem", func(st *api.Status) { st.Plugins[0].Problem = "login refused" }, nil, true},
		{"a plugin that needs setup", func(st *api.Status) { st.Plugins[0].NeedsSetup = true }, nil, true},
		{"a device's problem", func(st *api.Status) { st.Devices[1].Problem = "no LAN key" }, nil, true},
		{"a device's plugin not imported", func(st *api.Status) { st.Devices[1].PluginNotImported = true }, nil, true},

		// Nothing a command fixes.
		{"a device offline", func(st *api.Status) { st.Devices[0].Online = &off }, nil, false},
		{"a bridge with no controller", func(st *api.Status) { st.Matter.Fabrics = nil }, nil, false},
		{"a device not in your code", func(st *api.Status) { st.Devices[1].Declare = "home.Device(…)" }, nil, false},
		{"a deploy restarting the automations", func(st *api.Status) {
			st.Home.Process, st.Home.Deploying = false, true
		}, nil, false},
		{"Matter turned off", func(st *api.Status) { st.Matter = nil }, nil, false},

		// An offline device counts when it's named.
		{"an offline device named", func(st *api.Status) { st.Devices[0].Online = &off }, []string{"boiler"}, true},
		{"an offline device's capability named", func(st *api.Status) { st.Devices[0].Online = &off },
			[]string{"device/boiler/on"}, true},
		{"an offline device's room named", func(st *api.Status) { st.Devices[0].Online = &off },
			[]string{"room/utility"}, true},
		{"another device named", func(st *api.Status) { st.Devices[0].Online = &off }, []string{"hall-lamp"}, false},
		{"an offline device's plugin named", func(st *api.Status) { st.Devices[0].Online = &off },
			[]string{"plugin/opentherm"}, false},

		// What a ref names, and only that.
		{"a plugin named with its problem", func(st *api.Status) { st.Plugins[0].Problem = "login refused" },
			[]string{"plugin/opentherm"}, true},
		{"a device named whose plugin has a problem", func(st *api.Status) { st.Plugins[0].Problem = "login refused" },
			[]string{"hall-lamp"}, false},
		{"a device named with a problem", func(st *api.Status) { st.Devices[1].Problem = "no LAN key" },
			[]string{"room/hall"}, true},
		{"a flow named with automations down", func(st *api.Status) { st.Home.Process = false },
			[]string{"flow/morning"}, true},
		{"a device named with automations down", func(st *api.Status) { st.Home.Process = false },
			[]string{"hall-lamp"}, false},
		{"the bridge named while it's down", func(st *api.Status) { st.Matter.Serving = false }, []string{"matter"}, true},
		{"a device named while the bridge is down", func(st *api.Status) { st.Matter.Serving = false },
			[]string{"boiler"}, false},
	} {
		st := aHealthyHome()
		tc.break_(st)
		refs, err := statusRefs(tc.refs)
		if err != nil {
			t.Fatal(err)
		}
		if got := trouble(st, refs); (len(got) > 0) != tc.fails {
			t.Errorf("%s: trouble %q, want it to fail: %v", tc.name, got, tc.fails)
		}
	}
}

// --check prints what status does without it, and fails with nothing more to say.
func TestCheckPrintsTheSameAndExitsOne(t *testing.T) {
	broken := *aHealthyHome()
	broken.Plugins = slices.Clone(broken.Plugins)
	broken.Plugins[0].NeedsSetup = true
	for _, tc := range []struct {
		st   api.Status
		code int
	}{{*aHealthyHome(), 0}, {broken, 1}} {
		dir, _ := aHost(t, api.EventLog{}, nil, tc.st)
		out, errs, code := runOikade(t, cli.Plain, "status", "--data-dir", dir)
		checked, checkedErrs, checkedCode := runOikade(t, cli.Plain, "status", "--check", "--data-dir", dir)
		if code != 0 || checkedCode != tc.code || checked != out || checkedErrs != errs {
			t.Errorf("status --check exits %d, want %d:\n%s%s\nstatus exits %d:\n%s%s",
				checkedCode, tc.code, checked, checkedErrs, code, out, errs)
		}
	}
}

// With --json, which a person doesn't read, stderr says why --check failed. A rule's view says when
// your automations are down, which fails --check of it.
func TestCheckSaysWhyWhereTheOutputDoesnt(t *testing.T) {
	st := *aHealthyHome()
	st.Home.Process = false
	st.Plugins = []api.Plugin{{Name: "opentherm", Ref: "plugin/opentherm", NeedsSetup: true}}
	dir, _ := aHost(t, api.EventLog{}, nil, st)
	_, errs, code := runOikade(t, cli.Plain, "status", "--check", "--json", "--data-dir", dir)
	want := "Warning: automations down\nWarning: plugin/opentherm: needs setup\n"
	if code != 1 || errs != want {
		t.Errorf("status --check --json exits %d:\n%s\nwant 1, and stderr:\n%s", code, errs, want)
	}
	out, _, code := runOikade(t, cli.Plain, "status", "--check", "flow/morning", "--data-dir", dir)
	if code != 1 || !strings.Contains(out, "\nYour automations are down: ") {
		t.Errorf("status --check flow/morning exits %d:\n%s", code, out)
	}
}

// A ref that names nothing fails the command, even beside one that names something.
func TestStatusOfARefThatNamesNothingFails(t *testing.T) {
	st := *aHealthyHome()
	off := st
	off.Matter = nil
	for _, tc := range []struct {
		st   api.Status
		args []string
		want string
	}{
		{st, []string{"boilr"}, "Error: there's no device/boilr on record\nHint: oikade list devices lists them\n"},
		{st, []string{"boiler", "boilr"}, "Error: there's no device/boilr on record\n"},
		{
			st, []string{"device/boiler/setpoint"},
			"Error: device/boiler has no setpoint\nHint: oikade status boiler lists what it has\n",
		},
		{st, []string{"room/attic"}, "Error: no device is in room/attic\n"},
		{st, []string{"room/hall/temperature"}, "Error: no device in room/hall has temperature\n"},
		{st, []string{"flow/evening"}, "Error: there's no flow/evening in the running version\n"},
		{st, []string{"plugin/overkiz"}, "Error: there's no plugin/overkiz: your home doesn't import it"},
		{st, []string{"boilr", "flow/evening"}, "Error: device/boilr and flow/evening name nothing the host has\n"},
		{off, []string{"matter"}, "Error: this host has Matter turned off\n"},
	} {
		dir, _ := aHost(t, api.EventLog{}, nil, tc.st)
		out, errs, code := runOikade(t, cli.Plain, append([]string{"status", "--data-dir", dir}, tc.args...)...)
		if code != 1 || out != "" || !strings.HasPrefix(errs, tc.want) {
			t.Errorf("status %s exits %d:\n%s%s\nwant 1, and stderr starting:\n%s",
				strings.Join(tc.args, " "), code, out, errs, tc.want)
		}
	}
}

// status --json is one object, whatever narrows it: the home, and each kind, empty if no ref names
// it.
func TestStatusJSONKeepsItsShape(t *testing.T) {
	dir, _ := aHost(t, api.EventLog{}, nil, *aHealthyHome())
	keys := func(args ...string) (map[string]json.RawMessage, string) {
		out, errs, code := runOikade(t, cli.Plain, append([]string{"status", "--json", "--data-dir", dir}, args...)...)
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil {
			t.Fatalf("status --json %s exits %d: %v\n%s%s", strings.Join(args, " "), code, err, out, errs)
		}
		return got, strings.Join(slices.Sorted(maps.Keys(got)), " ")
	}
	_, whole := keys()
	if whole != "devices home matter plugins rules" {
		t.Errorf("status --json has %s", whole)
	}
	got, narrowed := keys("room/hall")
	if narrowed != whole {
		t.Errorf("status --json room/hall has %s, not %s", narrowed, whole)
	}
	for key, want := range map[string]string{"rules": "[]", "plugins": "[]", "matter": "null"} {
		if string(got[key]) != want {
			t.Errorf("status --json room/hall: %s is %s, not %s", key, got[key], want)
		}
	}
	var devices []api.Device
	if err := json.Unmarshal(got["devices"], &devices); err != nil || len(devices) != 1 || devices[0].Name != "hall-lamp" {
		t.Errorf("status --json room/hall: devices %s", got["devices"])
	}

	// A plugin brings its devices, and a capability only itself.
	got, _ = keys("plugin/opentherm", "device/hall-lamp/on", "matter")
	if err := json.Unmarshal(got["devices"], &devices); err != nil || len(devices) != 2 {
		t.Fatalf("devices %s", got["devices"])
	}
	if string(got["matter"]) == "null" || !strings.Contains(string(got["plugins"]), `"opentherm"`) {
		t.Errorf("matter %s, plugins %s", got["matter"], got["plugins"])
	}
}

// A device's view says since when it's offline, and ends with its last events, at most ten, without
// the home's own.
func TestADevicesViewEndsWithItsLastEvents(t *testing.T) {
	now := time.Now()
	st := *aHealthyHome()
	off, gone := false, now.Add(-3*time.Minute)
	st.Devices[0].Online, st.Devices[0].OfflineSince = &off, &gone
	events := make([]api.Event, 1, 13)
	events[0] = api.Event{Type: api.EventDeploy, Time: now.Add(-time.Hour), Message: "v3"}
	for i := range 12 {
		events = append(events, api.Event{
			Type: api.EventChange, Ref: "device/boiler/on", Capability: "on", Value: i%2 == 0,
			Time: now.Add(time.Duration(i-20) * time.Minute),
		})
	}
	dir, asked := aHost(t, api.EventLog{}, events, st)
	out, errs, code := runOikade(t, cli.Plain, "status", "boiler", "--data-dir", dir)
	if code != 0 || !strings.HasPrefix(out, "boiler in utility: went offline 3 min ago\n") {
		t.Fatalf("status boiler exits %d:\n%s%s", code, out, errs)
	}
	if n := strings.Count(out, "device/boiler/on  "); n != eventsShown || strings.Contains(out, "deployed") {
		t.Errorf("status boiler shows %d events, want %d without the deploy:\n%s", n, eventsShown, out)
	}
	// A day had enough of them, so it read no further back.
	if q := asked(); len(q) != 1 || !strings.Contains(q[0], "since=") {
		t.Errorf("asked the host for %q", q)
	}

	// With too few in the last day, it reads the whole log.
	dir, asked = aHost(t, api.EventLog{}, events[:3], st)
	if _, errs, code := runOikade(t, cli.Plain, "status", "boiler", "--data-dir", dir); code != 0 {
		t.Fatalf("status boiler exits %d: %s", code, errs)
	}
	if q := asked(); len(q) != 2 || strings.Contains(q[1], "since=") {
		t.Errorf("asked the host for %q", q)
	}
}
