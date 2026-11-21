package engine

import (
	"slices"
	"sync"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
)

// writeOrderEngine is an engine whose store has these devices: an arbitrated one's writes are
// claims, and the rest are plain writes.
func writeOrderEngine(t *testing.T, devices map[string]hub.Meta) *Engine {
	t.Helper()
	e := &Engine{Log: logging.Discard()}
	e.Store = hub.NewStore(logging.Discard())
	e.Init()
	m := map[ident.ID]hub.Meta{}
	for name, meta := range devices {
		meta.Instance = z2m
		m[ident.ID{Name: name}] = meta
	}
	e.Store.SetDevices(z2m, m)
	return e
}

var arbitratedOn = []control.Entry{entry(capability.On, "false", control.NameOverride, "1h")}

// queuedWrite is a hold write that says when it runs.
type queuedWrite struct {
	rule  string
	level control.Level
	to    HoldTarget
	ends  bool
}

// runWrites queues these writes in this order, as one look's evaluates would, and returns the order
// they ran in.
func runWrites(t *testing.T, e *Engine, writes ...queuedWrite) []string {
	t.Helper()
	var mu sync.Mutex
	var ran []string
	var done sync.WaitGroup
	e.mu.Lock()
	for _, w := range writes {
		done.Add(1)
		e.holdWrites = append(e.holdWrites, holdWrite{
			level: w.level, key: ident.ID{Name: w.rule}, target: w.to, ends: w.ends,
			run: func() {
				defer done.Done()
				mu.Lock()
				ran = append(ran, w.rule)
				mu.Unlock()
			},
		})
	}
	e.startWrites(t.Context())
	e.mu.Unlock()
	waitGroup(t, &done)
	return ran
}

func waitGroup(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the writes never all ran")
	}
}

// Claims go in highest level first, so the arbiter never writes a lower claim's value on the way.
func TestClaimsGoHighestLevelFirst(t *testing.T) {
	e := writeOrderEngine(t, map[string]hub.Meta{"heater": {Control: arbitratedOn}})
	heater := HoldTarget{Device: "heater", Capability: capability.On}
	got := runWrites(t, e,
		queuedWrite{"warm", control.Normal, heater, false},
		queuedWrite{"cutoff", control.Safety, heater, false},
		queuedWrite{"boost", control.Override, heater, false},
	)
	if want := []string{"cutoff", "boost", "warm"}; !slices.Equal(got, want) {
		t.Errorf("claims ran in the order %v, want %v", got, want)
	}
}

// A claim that ends goes after those that start, whatever their levels, so a hold that takes over
// at the same moment has claimed before the arbiter is left with nothing but the default.
func TestClaimsStartBeforeOthersEnd(t *testing.T) {
	e := writeOrderEngine(t, map[string]hub.Meta{"pump": {Control: arbitratedOn}})
	pump := HoldTarget{Device: "pump", Capability: capability.On}
	got := runWrites(t, e,
		queuedWrite{"a-frost", control.Safety, pump, true},
		queuedWrite{"b-budget", control.Normal, pump, true},
		queuedWrite{"c-swimming", control.Normal, pump, false},
		queuedWrite{"d-boost", control.Override, pump, false},
	)
	if want := []string{"d-boost", "c-swimming", "a-frost", "b-budget"}; !slices.Equal(got, want) {
		t.Errorf("claims ran in the order %v, want %v", got, want)
	}
}

// A plain end writes the opposite of its value, so it goes before the starts, and theirs stay.
func TestPlainWritesEndBeforeOthersStart(t *testing.T) {
	e := writeOrderEngine(t, map[string]hub.Meta{"lamp": {}})
	lamp := HoldTarget{Device: "lamp", Capability: capability.On}
	got := runWrites(t, e,
		queuedWrite{"a-evening", control.Normal, lamp, false},
		queuedWrite{"b-cutoff", control.Safety, lamp, true},
		queuedWrite{"c-dusk", control.Normal, lamp, true},
	)
	if want := []string{"c-dusk", "b-cutoff", "a-evening"}; !slices.Equal(got, want) {
		t.Errorf("plain writes ran in the order %v, want %v", got, want)
	}
}

// Plain writes have nothing to arbitrate them, so they go lowest level first and the highest
// level's value is the one that stays.
func TestPlainWritesGoLowestLevelFirst(t *testing.T) {
	e := writeOrderEngine(t, map[string]hub.Meta{"lamp": {}})
	lamp := HoldTarget{Device: "lamp", Capability: capability.On}
	got := runWrites(t, e,
		queuedWrite{"cutoff", control.Safety, lamp, false},
		queuedWrite{"warm", control.Normal, lamp, false},
		queuedWrite{"boost", control.Override, lamp, false},
	)
	if want := []string{"warm", "boost", "cutoff"}; !slices.Equal(got, want) {
		t.Errorf("plain writes ran in the order %v, want %v", got, want)
	}
}

// In one room, claims go before plain writes, whatever their levels.
func TestARoomsClaimsGoBeforeItsPlainWrites(t *testing.T) {
	e := writeOrderEngine(t, map[string]hub.Meta{
		"heater": {Room: "hall", Control: arbitratedOn},
		"lamp":   {Room: "hall"},
	})
	got := runWrites(t, e,
		queuedWrite{"lamp-cutoff", control.Safety, HoldTarget{Device: "lamp", Capability: capability.On}, false},
		queuedWrite{"heater-warm", control.Normal, HoldTarget{Device: "heater", Capability: capability.On}, false},
	)
	if want := []string{"heater-warm", "lamp-cutoff"}; !slices.Equal(got, want) {
		t.Errorf("the hall's writes ran in the order %v, want %v", got, want)
	}
}

// Writes that can reach one room run one after another, a room's own and its devices', and another
// room's run alongside, so a slow device holds up only its own room.
func TestARoomsWritesWaitForEachOtherAndNotForAnotherRoom(t *testing.T) {
	e := writeOrderEngine(t, map[string]hub.Meta{
		"heater": {Room: "hall"},
		"lamp":   {Room: "hall"},
		"fan":    {Room: "garage"},
	})
	garageRan := make(chan struct{})
	var lampStarted, hallClash bool
	var mu sync.Mutex
	var done sync.WaitGroup
	done.Add(3)
	e.mu.Lock()
	e.holdWrites = []holdWrite{
		{level: control.Normal, key: ident.ID{Name: "a-hall"}, target: HoldTarget{Room: "hall", Capability: capability.On}, run: func() {
			defer done.Done()
			// The hall's first write is slow: the garage's goes ahead meanwhile.
			select {
			case <-garageRan:
			case <-time.After(5 * time.Second):
				t.Error("the garage's write waited for the hall's")
			}
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			hallClash = lampStarted
			mu.Unlock()
		}},
		{level: control.Normal, key: ident.ID{Name: "b-lamp"}, target: HoldTarget{Device: "lamp", Capability: capability.On}, run: func() {
			defer done.Done()
			mu.Lock()
			lampStarted = true
			mu.Unlock()
		}},
		{level: control.Normal, key: ident.ID{Name: "c-garage"}, target: HoldTarget{Room: "garage", Capability: capability.On}, run: func() {
			defer done.Done()
			close(garageRan)
		}},
	}
	e.startWrites(t.Context())
	e.mu.Unlock()
	waitGroup(t, &done)
	if hallClash {
		t.Error("the hall's second write ran while its first was still under way")
	}
}
