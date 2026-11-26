package oikade

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/wire"
)

// heater and light are devices as oikade types writes them.
type heater struct {
	Hardware
	On       Bool
	Setpoint Number
}

func newHeater(name string) heater {
	return heater{Hardware{Key: name}, Bool{Device: name, Capability: On}, Number{Device: name, Capability: Setpoint}}
}

type light struct {
	Hardware
	On Bool
}

func newLight(name string) light {
	return light{Hardware{Key: name}, Bool{Device: name, Capability: On}}
}

var (
	hallHeater, loftHeater = newHeater("hall-heater"), newHeater("loft-heater")
	hallLamp               = newLight("hall-lamp")
)

func TestCompileRejectsAnUnsoundGroup(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	for name, tc := range map[string]struct {
		home func(h *Home)
		want string
	}{
		"no member":      {func(h *Home) { h.Group("g") }, `group "g": it needs a member`},
		"a member twice": {func(h *Home) { h.Group("g", hallLamp, hallLamp) }, "hall-lamp is a member twice"},
		"a nil member":   {func(h *Home) { h.Group("g", nil) }, "it has a nil member"},
		"a nameless one": {func(h *Home) { h.Group("g", light{}) }, "a member with no name"},
		"a space":        {func(h *Home) { h.Group("a b", hallLamp) }, `"a b" isn't a group name`},
		"its own name":   {func(h *Home) { h.Group("hall-lamp", hallLamp) }, "a member of its own name"},
		"a device's name": {
			func(h *Home) { h.Group("hall-heater", hallLamp); h.Group("g", hallHeater) },
			`group "hall-heater" has a device's name`,
		},
		"made twice":       {func(h *Home) { h.Group("g", hallLamp); h.Group("g", hallHeater) }, `group "g" is made twice`},
		"a room with none": {func(*Home) { Room("") }, "a room needs a name"},
		"no member has it": {
			func(h *Home) { h.Decide(h.Group("g", hallLamp).Setpoint).Default(18) },
			"no member of group g has setpoint",
		},
		"read as one": {
			func(h *Home) { h.Decide(hallLamp.On).Case("a", true, h.Group("g", hallHeater).On).Default(false) },
			"group g on is a group's: read it with Any or All",
		},
		"Any of a device": {
			func(h *Home) { h.Decide(hallLamp.On).Case("a", true, hallHeater.On.Any()).Default(false) },
			"Any of hall-heater on needs a group's",
		},
		"decided on its own too": {
			func(h *Home) {
				g := h.Group("g", hallHeater, loftHeater)
				h.Decide(g.On).Default(false)
				h.Decide(hallHeater.On).Case("a", true, when).Default(false)
			},
			"hall-heater on is decided on its own and in group g: hand its decision over with Then(group g on)",
		},
		"in two decided groups": {
			func(h *Home) {
				h.Decide(h.Group("g", hallHeater).On).Default(false)
				h.Decide(h.Group("f", hallHeater, loftHeater).On).Default(false)
			},
			"hall-heater on is in group g and group f, which both decide it",
		},
		"Then to an undecided group": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(hallHeater.On).Case("a", true, when).Then(g.On)
			},
			"Then(group g on) has no decision",
		},
		"Then to a device": {
			func(h *Home) { h.Decide(hallHeater.On).Then(loftHeater.On) },
			"Then(loft-heater on) needs a group's",
		},
		"Then to a group it isn't in": {
			func(h *Home) {
				g := h.Group("g", loftHeater)
				h.Decide(g.On).Default(false)
				h.Decide(hallHeater.On).Then(g.On)
			},
			"hall-heater isn't a member with on",
		},
		"Then to another capability": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(g.Setpoint).Default(18)
				h.Decide(hallHeater.Setpoint).Then(g.Brightness)
			},
			"Then(group g brightness) is another capability",
		},
		"Then and Default": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(g.On).Default(false)
				h.Decide(hallHeater.On).Then(g.On).Default(true)
			},
			"Default comes after Then",
		},
		"a case after Then": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(g.On).Default(false)
				h.Decide(hallHeater.On).Then(g.On).Case("a", true, when)
			},
			`Case "a" comes after Then`,
		},
		"Limits with Then": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(g.On).Default(false)
				h.Decide(hallHeater.On).Limits(Limits{MinimumOn: time.Hour}).Then(g.On)
			},
			"Limits are the group's",
		},
		"a group's Then": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(g.On).Then(g.On)
			},
			"a group's decision needs a Default",
		},
		"a group's budget": {
			func(h *Home) {
				h.Decide(h.Group("g", hallHeater).On).Budget("b", true, NewBudget(func(Snapshot) int { return 1 })).Default(false)
			},
			"a budget is per device",
		},
		"a group's average": {
			func(h *Home) {
				g := h.Group("g", hallHeater)
				h.Decide(hallLamp.On).Case("a", true, g.Setpoint.AverageOver(time.Hour).Above(3)).Default(false)
			},
			"has no one reading to average",
		},
		"a trigger on what no member has": {
			func(h *Home) { h.Flow("f", nothing).When(h.Group("g", hallLamp).Setpoint) },
			"When[0]: no member of group g has setpoint",
		},
	} {
		t.Run(name, func(t *testing.T) {
			declaring(t)
			_, _, err := homeWith(Options{}, tc.home).compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// controlOf is the manifest's settings, by device or room.
func controlOf(m wire.Manifest) map[string]wire.DeviceControl {
	out := map[string]wire.DeviceControl{}
	for _, c := range m.Control {
		out[c.Device+c.Room+" "+c.Entry.Capability] = c
	}
	return out
}

// A group's decision is each member's that has the capability, and a member that hands over with
// Then ranks its own lines above the group's, with the group's override and Default.
func TestAGroupsDecisionIsEachMembersSetting(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	h := decide(func(h *Home) {
		g := h.Group("heaters", hallHeater, loftHeater, hallLamp)
		h.Decide(g.Setpoint).Case("frost", 7, when).OverrideFor(time.Hour).Case("day", 20, when).Default(16)
		h.Decide(hallHeater.Setpoint).Case("busy", 21, when).Then(g.Setpoint)
	})
	m := h.manifest()
	control := controlOf(m)
	if len(control) != 2 {
		t.Fatalf("got settings %v, want the two heaters'", slices.Collect(func(yield func(string) bool) {
			for k := range control {
				if !yield(k) {
					return
				}
			}
		}))
	}
	loft := control["loft-heater setpoint"].Entry
	group := []string{"Rule/heaters setpoint frost", "override", "Rule/heaters setpoint day"}
	if !slices.Equal(loft.Lines, group) {
		t.Errorf("loft-heater's lines are %v, want %v", loft.Lines, group)
	}
	hall := control["hall-heater setpoint"].Entry
	if want := append([]string{"Rule/hall-heater setpoint busy"}, group...); !slices.Equal(hall.Lines, want) {
		t.Errorf("hall-heater's lines are %v, want %v", hall.Lines, want)
	}
	if string(hall.Default) != "16" || hall.External != loft.External ||
		hall.OverrideHold != loft.OverrideHold || hall.OverrideHold == "" {
		t.Errorf("hall-heater's entry is %+v, want the group's Default and override", hall)
	}

	rules := map[string]wire.RuleInfo{}
	for _, r := range m.Rules {
		rules[r.Name] = r
	}
	if !rules["hall-heater setpoint busy"].Safety || !rules["heaters setpoint frost"].Safety ||
		rules["heaters setpoint day"].Safety {
		t.Errorf("got rules %+v", m.Rules)
	}

	frost := caseNamed(t, h, "heaters setpoint frost")
	if got := frost.Devices; !slices.Equal(got, []string{"hall-heater", "loft-heater"}) {
		t.Errorf("the group's hold writes %v", got)
	}
}

// caseNamed is the hold of a home's case with that key.
func caseNamed(t *testing.T, h *Home, key string) engine.Hold {
	t.Helper()
	decided, err := h.compileDecisions()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decided {
		for _, c := range d.cases {
			if c.key == key {
				return c.hold
			}
		}
	}
	t.Fatalf("no case %q", key)
	return engine.Hold{}
}

// A room's decision is one setting for the room, and its holds leave out the devices with decisions
// of their own, unless they hand over to the room's.
func TestARoomsDecisionLeavesOutDevicesWithTheirOwn(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	h := decide(func(h *Home) {
		hall := h.Room(Room("hall"), Lights)
		h.Decide(hall.On).Case("alarm", true, when).OverrideFor(time.Hour).Case("evening", true, when).Default(false)
		h.Decide(hallLamp.On).Case("reading", true, when).Default(false)
		h.Decide(newLight("hall-spot").On).Case("art", true, when).Then(hall.On)
	})
	control := controlOf(h.manifest())
	room := control["hall on"]
	lights := []devicetype.Type{devicetype.OnOffLight, devicetype.DimmableLight}
	if room.Room != "hall" || !slices.Equal(room.DeviceTypes, lights) {
		t.Errorf("the room's setting is %+v", room)
	}
	if _, ok := control["hall-lamp on"]; !ok {
		t.Error("hall-lamp has no setting of its own")
	}
	var info wire.RuleInfo
	for _, r := range h.manifest().Rules {
		if r.Name == "hall on alarm" {
			info = r
		}
	}
	want := []wire.RoomOfTypes{{Room: "hall", Capability: "on", DeviceTypes: lights, Except: []string{"hall-lamp"}}}
	if !reflect.DeepEqual(info.Rooms, want) {
		t.Errorf("the room's Safety case writes %+v, want %+v", info.Rooms, want)
	}
	evening := caseNamed(t, h, "hall on evening")
	wantHold := engine.Hold{Room: "hall", DeviceTypes: lights, Except: []string{"hall-lamp"}}
	got := engine.Hold{Room: evening.Room, DeviceTypes: evening.DeviceTypes, Except: evening.Except}
	if !reflect.DeepEqual(got, wantHold) {
		t.Errorf("the room's hold is %+v, want %+v", got, wantHold)
	}
}

// A flow's write to a group is one to each member with the capability.
func TestAGroupsSetIsEachMembers(t *testing.T) {
	g := NewHome(Options{}).Group("g", hallHeater, hallLamp, loftHeater)
	s := g.Setpoint.To(19)
	if !slices.Equal(s.devices, []string{"hall-heater", "loft-heater"}) {
		t.Errorf("a Set of the group's setpoint is to %v", s.devices)
	}
	got := g.On.changes()
	if len(got) != 3 || !reflect.DeepEqual(got[1], engine.Change{Device: "hall-lamp", Capability: "on"}) {
		t.Errorf("a change of the group's on is %+v", got)
	}
}

// Any and All of a group read its members that have reported.
func TestAGroupsAnyAndAllReadItsMembers(t *testing.T) {
	g := NewHome(Options{}).Group("g", hallHeater, hallLamp, loftHeater)
	vars := engine.Vars{"devices": map[string]map[string]any{
		"hall-heater": {"on": true}, "hall-lamp": {"on": false}, "loft-heater": {},
	}}
	for name, tc := range map[string]struct {
		c    Condition
		want bool
	}{
		"any": {g.On.Any(), true},
		"all": {g.On.All(), false},
	} {
		ok, err := toEngine(tc.c)(vars)
		if err != nil || ok != tc.want {
			t.Errorf("%s: got %v, %v; want %v", name, ok, err, tc.want)
		}
	}
	readings := toEngine(ConditionFunc("r", func(s Snapshot) bool { return s.Count(g.On) == 1 }))
	if ok, err := readings(vars); err != nil || !ok {
		t.Errorf("Count: got %v, %v", ok, err)
	}
	if _, err := toEngine(g.On.Any())(engine.Vars{"devices": map[string]map[string]any{}}); err == nil {
		t.Error("a group where nothing has reported is told")
	}
}

// A room made twice is one, so a Then finds its decision, and a room deciding what a device of its
// name decides is refused, since their cases would be named alike.
func TestARoomMadeTwiceIsOne(t *testing.T) {
	when := ConditionFunc("always", func(Snapshot) bool { return true })
	h := decide(func(h *Home) {
		h.Decide(h.Room(Room("hall"), Lights).On).Default(false)
		h.Decide(hallLamp.On).Case("a", true, when).Then(h.Room(Room("hall"), Lights).On)
	})
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	porch := newLight("porch")
	h = decide(func(h *Home) {
		h.Decide(porch.On).Default(false)
		h.Decide(Room("porch").On).Default(false)
	})
	_, _, err := h.compile()
	want := "porch on and room porch on are both decided, and would name their cases alike"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// A typed nil member is a mistake, not a panic.
func TestATypedNilMemberIsAMistake(t *testing.T) {
	var none *light
	_, _, err := decide(func(h *Home) { h.Group("g", none) }).compile()
	if err == nil || !strings.Contains(err.Error(), "it has a nil member") {
		t.Fatalf("got %v", err)
	}
}
