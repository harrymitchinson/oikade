package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// OIK-215: a hold's state shows only as a claim on a device otherwise, and why it stopped not at all.
func TestStatusHasEachHoldsStateAndWhyItStopped(t *testing.T) {
	dir := tempDir(t)
	socket, _ := startHost(t, dir)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, _, err := wire.Dial(ctx, socket, []string{"lamp-on", "heater", "rail"}, nil,
		wire.RuleInfo{Name: "lamp-on", Kind: wire.KindHold},
		wire.RuleInfo{Name: "heater", Kind: wire.KindHold},
		wire.RuleInfo{Name: "rail", Kind: wire.KindHold},
		wire.RuleInfo{Name: "gate", Kind: "moment"},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	go c.Listen(wire.Events{Changed: func(wire.Message) {}, Discovered: func(wire.Device) {}, Ping: func(uint64) {}})

	since := time.Date(2026, 11, 7, 23, 7, 5, 0, time.UTC)
	offAt := since.Add(3 * time.Minute)
	if err := c.Put(ctx, ident.ID{Name: "lamp-on"}, engine.HoldState{
		Active: true, Since: since, OffAt: offAt, Latched: true,
		Target: engine.HoldTarget{Device: "lamp", Capability: "on", Value: true},
	}); err != nil {
		t.Fatal(err)
	}
	budget, err := json.Marshal(map[string]any{"today": engine.BudgetDay{
		Date: "2026-11-07", TargetMinutes: 120, ScheduledMinutes: 90, RunMinutes: 45,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, ident.ID{Name: "rail"}, engine.HoldState{Budget: string(budget)}); err != nil {
		t.Fatal(err)
	}
	c.Stopped("heater", "no device in room hall has reported occupancy")

	local := api.Local{DataDir: dir}
	rules := waitForRules(ctx, t, local, func(r map[string]api.Rule) bool { return r["heater"].Stopped != "" })
	lamp := rules["lamp-on"]
	if lamp.Active == nil || !*lamp.Active || !lamp.Since.Equal(since) || !lamp.OffAt.Equal(offAt) ||
		!lamp.OnAt.IsZero() || !lamp.Latched || lamp.Stopped != "" || lamp.Budget != nil {
		t.Errorf("lamp-on: got %+v", lamp)
	}
	heater := rules["heater"]
	if heater.Active == nil || *heater.Active || heater.Stopped != "no device in room hall has reported occupancy" {
		t.Errorf("heater: got %+v", heater)
	}
	rail := rules["rail"]
	if b := rail.Budget; b == nil || *b != (api.RuleBudget{Date: "2026-11-07", TargetMinutes: 120, PlannedMinutes: 90, RunMinutes: 45}) {
		t.Errorf("rail: got budget %+v", rail.Budget)
	}
	if gate := rules["gate"]; gate.Active != nil {
		t.Errorf("a moment has a hold's state: %+v", gate)
	}

	// It's sent again once the hold can tell.
	c.Stopped("heater", "")
	waitForRules(ctx, t, local, func(r map[string]api.Rule) bool { return r["heater"].Stopped == "" })

	// A paused hold has no state: its process isn't running it.
	if resp, err := api.Call(ctx, local, api.Request{Type: api.TypePause, Rule: "lamp-on"}); err != nil || resp.Error != "" {
		t.Fatalf("pause: got %+v, %v", resp, err)
	}
	if lamp := waitForRules(ctx, t, local, func(r map[string]api.Rule) bool { return r["lamp-on"].Paused })["lamp-on"]; lamp.Active != nil {
		t.Errorf("a paused hold has a state: %+v", lamp)
	}
}

// waitForRules returns the status's rules by name once done says they're as wanted.
func waitForRules(ctx context.Context, t *testing.T, local api.Local, done func(map[string]api.Rule) bool) map[string]api.Rule {
	t.Helper()
	for {
		resp, err := api.Call(ctx, local, api.Request{Type: api.TypeStatus})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status == nil {
			t.Fatalf("got %+v", resp)
		}
		rules := map[string]api.Rule{}
		for _, r := range resp.Status.Rules {
			rules[r.Name] = r
		}
		if done(rules) {
			return rules
		}
		if ctx.Err() != nil {
			t.Fatalf("got rules %+v", resp.Status.Rules)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
