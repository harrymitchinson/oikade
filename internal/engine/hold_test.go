/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/sun"
)

// ruleYAML decodes a Rule spec as kubectl would.
func ruleYAML(t *testing.T, spec string) *homev1alpha1.Rule {
	t.Helper()
	var r homev1alpha1.Rule
	if err := yaml.UnmarshalStrict([]byte("metadata: {name: r, namespace: default}\nspec:\n"+spec), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestCompileHold(t *testing.T) {
	c, err := Compile(ruleYAML(t, `
  priority: Safety
  while:
    window: {start: {sun: Sunset, offset: -15m}, end: "23:30", days: [Fri, Sat]}
    when: devices["workshop-temperature"]["sensor.temperature"] > 45
    onDelay: 1m
    offDelay: 5m
    latch: true
  hold: {device: workshop-heater, capability: switch.on, value: false}
`))
	if err != nil {
		t.Fatal(err)
	}
	h := c.hold
	if h == nil {
		t.Fatal("no hold")
	}
	if h.device != "workshop-heater" || h.capability != "switch.on" || h.value != false || h.priority != homev1alpha1.RulePrioritySafety {
		t.Errorf("hold target: %+v", h)
	}
	if h.when == nil || !h.latch || h.onDelay != time.Minute || h.offDelay != 5*time.Minute {
		t.Errorf("hold timing: %+v", h)
	}
	w := h.window
	if w.start.event != sun.Sunset || w.start.offset != -15*time.Minute || w.end.minutes != 23*60+30 {
		t.Errorf("window: %+v", w)
	}
	if len(w.days) != 2 || w.days[0] != time.Friday {
		t.Errorf("days: %v", w.days)
	}
	if len(c.changes) != 0 || len(c.timers) != 0 || len(c.actions) != 0 {
		t.Error("a hold has no triggers or actions")
	}
}

func TestWindowTimeRoundTrips(t *testing.T) {
	for _, in := range []string{`"06:00"`, `{"sun":"Sunrise","offset":"30m0s"}`} {
		var w homev1alpha1.WindowTime
		if err := w.UnmarshalJSON([]byte(in)); err != nil {
			t.Fatal(err)
		}
		out, err := w.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Errorf("%s round-tripped to %s", in, out)
		}
	}
}

func TestCompileRejectsHolds(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"no hold": {`
  while: {when: "true"}`, "hold is required with while"},
		"triggers and while": {`
  triggers: [{at: {time: "07:00"}}]
  do: [{delay: 1m}]
  while: {when: "true"}
  hold: {device: d, capability: switch.on, value: true}`, "exactly one of triggers or while"},
		"do with while": {`
  while: {when: "true"}
  hold: {device: d, capability: switch.on, value: true}
  do: [{delay: 1m}]`, "do is only allowed with triggers"},
		"bad time": {`
  while: {window: {start: "6:00", end: "08:00"}}
  hold: {device: d, capability: switch.on, value: true}`, `while.window: start: time "6:00" is not HH:MM`},
		"time and sun": {`
  while: {window: {start: {sun: Sunset, offset: 1h}, end: {sun: Noon}}}
  hold: {device: d, capability: switch.on, value: true}`, `while.window: end: unknown event "Noon"`},
		"offset on a time": {`
  while: {window: {start: {offset: 1h}, end: "08:00"}}
  hold: {device: d, capability: switch.on, value: true}`, `exactly one of "HH:MM" or {sun, offset}`},
		"when not a bool": {`
  while: {when: "1 + 1"}
  hold: {device: d, capability: switch.on, value: true}`, "while.when: must evaluate to a bool"},
		"when has no trigger": {`
  while: {when: "trigger.value"}
  hold: {device: d, capability: switch.on, value: true}`, "while.when: "},
		"latch without when": {`
  while: {window: {start: "06:00", end: "08:00"}, latch: true}
  hold: {device: d, capability: switch.on, value: true}`, "latch needs when"},
		"read-only hold": {`
  while: {when: "true"}
  hold: {device: d, capability: sensor.temperature, value: 20}`, "hold: capability sensor.temperature is read-only"},
		"wrong kind": {`
  while: {when: "true"}
  hold: {room: r, capability: light.level, value: true}`, "hold: light.level takes a number"},
		"for with released": {`
  triggers: [{at: {time: "07:00"}}]
  do: [{set: {device: d, capability: switch.on, value: true, for: 1h, until: Released}}]`, "for can't be combined with until: Released"},
		"zero for": {`
  triggers: [{at: {time: "07:00"}}]
  do: [{set: {device: d, capability: switch.on, value: true, for: 0s}}]`, "for must be positive"},
		"release without a rule": {`
  triggers: [{at: {time: "07:00"}}]
  do: [{release: {rule: ""}}]`, "release needs a rule"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(ruleYAML(t, tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCompileClaims(t *testing.T) {
	c, err := Compile(ruleYAML(t, `
  triggers: [{change: {device: b, capability: sensor.contact-open}}]
  do:
  - set: {device: rail, capability: switch.on, value: true, for: 1h, until: Next, priority: Override}
  - set: {device: rail, capability: switch.on, value: false, until: Released}
  - release: {rule: other}
`))
	if err != nil {
		t.Fatal(err)
	}
	if a := c.actions[0]; a.claimFor != time.Hour || a.until != homev1alpha1.ClaimUntilNext || a.priority != homev1alpha1.RulePriorityOverride {
		t.Errorf("first set: %+v", a)
	}
	if a := c.actions[1]; a.until != homev1alpha1.ClaimUntilReleased {
		t.Errorf("second set: %+v", a)
	}
	if a := c.actions[2]; a.release != "other" {
		t.Errorf("release: %+v", a)
	}
}

func TestReleaseActionsDoNothingYet(t *testing.T) {
	setter := &fakeSetter{}
	result, _ := runRoom(t, setter, nil,
		homev1alpha1.Action{Release: &homev1alpha1.ReleaseAction{Rule: "workshop-overheat"}},
		set("hallway-light", "light.on", "true"))
	if result != ResultSucceeded {
		t.Fatalf("result = %s, want %s", result, ResultSucceeded)
	}
	if got := setter.got(); len(got) != 1 || got[0].device != "hallway-light" {
		t.Fatalf("calls = %v, want only the set", got)
	}
}
