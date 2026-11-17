package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// A rule that runs elsewhere ends its claims as a flow does: all but its Safety ones.
func TestReleaseRuleEndsEveryClaimButSafetyOnes(t *testing.T) {
	eng, dev := arbitratedEngine(t, newMemoryStates[ControlState]())
	other := ident.ID{Name: "other"}
	meta := hub.Meta{Instance: z2m, Control: []control.Entry{
		entry(capability.On, "true", control.NameOverride, "1h"),
	}}
	eng.Store.SetDevices(z2m, map[ident.ID]hub.Meta{dev: meta, other: meta})
	ctx := context.Background()
	key := goKey("routine")
	expires := time.Now().Add(time.Hour)
	for d, level := range map[ident.ID]control.Level{dev: control.Override, other: control.Safety} {
		claim := control.Claim{Source: ruleSource(key.Name), Level: level, Expires: expires}
		if _, err := eng.Claim(ctx, d, capability.On, claim); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.ReleaseRule(ctx, key); err != nil {
		t.Fatal(err)
	}
	claims := func(d ident.ID) int { return len(eng.arbs[d][capability.On].a.State().Claims) }
	if claims(dev) != 0 {
		t.Error("an Override claim stayed")
	}
	if claims(other) != 1 {
		t.Error("a Safety claim was released")
	}
}

// A device with no arbiters yet counts the claims on record that haven't expired.
func TestSavedClaimsLeaveOutExpiredOnes(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	controls := newMemoryStates[ControlState]()
	dev := ident.ID{Name: "valve"}
	if err := controls.Put(context.Background(), dev, ControlState{capability.On: {Claims: []control.Claim{
		{Source: "Rule/gone", Level: control.Safety, Expires: now},
		{Source: "Rule/later", Level: control.Normal, Expires: now.Add(time.Minute)},
		{Source: "Rule/until-released", Level: control.Safety},
	}}}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Clock: clock.NewFake(now), Controls: controls}
	saved := e.SavedClaims(dev)
	sources := make([]string, 0, len(saved))
	for _, c := range saved {
		sources = append(sources, c.Source)
	}
	if len(sources) != 2 || sources[0] != "Rule/later" || sources[1] != "Rule/until-released" {
		t.Errorf("saved claims %v, want Rule/later and Rule/until-released", sources)
	}
	if _, ok := e.SavedSafety(dev, capability.On, 1); !ok {
		t.Error("the Safety claim that never expires didn't count")
	}
}

// unreadableControls has a device whose saved claims can't be read.
type unreadableControls struct {
	*memoryStates[ControlState]
	dev ident.ID
}

func (u unreadableControls) Unreadable(key ident.ID) error {
	if key == u.dev {
		return errors.New("bad record")
	}
	return nil
}

// Saved claims that can't be read count as a Safety claim on the whole device, because they might
// be one.
func TestUnreadableClaimsHoldTheDeviceAtSafety(t *testing.T) {
	dev, fine := ident.ID{Name: "valve"}, ident.ID{Name: "lamp"}
	e := &Engine{Clock: clock.NewFake(time.Now()), Controls: unreadableControls{newMemoryStates[ControlState](), dev}}
	c, ok := e.UnreadableClaim(dev)
	if !ok || c.Level != control.Safety || c.Source != UnreadableClaims {
		t.Fatalf("an unreadable record's claim: %+v, %v", c, ok)
	}
	if got := e.SavedClaims(dev); len(got) != 1 || got[0] != c {
		t.Errorf("saved claims %+v, want only the unreadable claim", got)
	}
	if got, ok := e.SavedSafety(dev, capability.On, 0); !ok || got != c {
		t.Errorf("saved Safety %+v, %v, want the unreadable claim", got, ok)
	}
	if _, ok := e.UnreadableClaim(fine); ok {
		t.Error("a device whose record reads fine is held")
	}
}
