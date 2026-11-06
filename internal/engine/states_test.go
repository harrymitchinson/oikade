package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/store"
)

// storeBackend saves states in the embedded store. holds and controls return them as a restarted
// process finds them, so a test restarts by asking for them again, holds first.
type storeBackend struct {
	dir string
	s   *store.Store
}

func newStoreBackend(t *testing.T) *storeBackend {
	t.Helper()
	b := &storeBackend{dir: t.TempDir()}
	t.Cleanup(func() {
		if b.s != nil {
			_ = b.s.Close()
		}
	})
	return b
}

// reopen closes the store and opens it again, as a restart does.
func (b *storeBackend) reopen(t *testing.T) {
	t.Helper()
	if b.s != nil {
		if err := b.s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	b.s = s
}

func (b *storeBackend) holds(t *testing.T) HoldStates {
	t.Helper()
	b.reopen(t)
	return &Stored[HoldState]{Values: b.s.Namespace(HoldStatesNamespace), Log: logr.Discard()}
}

func (b *storeBackend) controls(t *testing.T) ControlStates {
	t.Helper()
	if b.s == nil {
		b.reopen(t)
	}
	return &Stored[ControlState]{Values: b.s.Namespace(ControlStatesNamespace), Log: logr.Discard()}
}

func (b *storeBackend) corrupt(t *testing.T, key types.NamespacedName) {
	t.Helper()
	if b.s == nil {
		b.reopen(t)
	}
	if err := b.s.Namespace(HoldStatesNamespace).Put(key.String(), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
}

// restartTests are the engine's tests that stop it and start it again from saved state.
var restartTests = map[string]func(*testing.T, *storeBackend){
	"WindowHoldRestartRespectsATap":              testWindowHoldRestartRespectsATap,
	"WindowHoldStartsAfterDowntime":              testWindowHoldStartsAfterDowntime,
	"OffDelaySurvivesARestart":                   testOffDelaySurvivesARestart,
	"MotionOffDelaySurvivesARestart":             testMotionOffDelaySurvivesARestart,
	"LatchedHoldActsOnStartupUntilReleased":      testLatchedHoldActsOnStartupUntilReleased,
	"Scenario7BoostEndsAtTheFirstOfForAndNext":   testScenario7BoostEndsAtTheFirstOfForAndNext,
	"Scenario8LeakClosesTheValveUntilReleased":   testScenario8LeakClosesTheValveUntilReleased,
	"Scenario5PumpBudgetWithFrostAndAHomeAppTap": testScenario5PumpBudgetWithFrostAndAHomeAppTap,
}

func TestRestarts(t *testing.T) {
	for name, test := range restartTests {
		t.Run(name, func(t *testing.T) { test(t, newStoreBackend(t)) })
	}
}

func TestStatesKeepHoldsAcrossARestart(t *testing.T) {
	b := newStoreBackend(t)
	ctx := t.Context()
	kept := types.NamespacedName{Namespace: ns, Name: "kept"}
	other := types.NamespacedName{Namespace: "other", Name: "kept"}
	deleted := types.NamespacedName{Namespace: ns, Name: "deleted"}
	want := HoldState{
		Active: true, Latched: true, OnAt: time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC),
		Target: HoldTarget{Device: "main-valve", Capability: "switch.on", Value: false},
	}
	s := b.holds(t)
	if _, ok := s.Get(kept); ok {
		t.Error("a new store has a state")
	}
	for key, state := range map[types.NamespacedName]HoldState{kept: want, other: {Active: true}, deleted: want} {
		if err := s.Put(ctx, key, state); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := s.Get(kept); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("before the restart: %+v, %v", got, ok)
	}
	if err := s.Delete(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(deleted); ok {
		t.Error("the deleted state is still there")
	}

	s = b.holds(t)
	if got, ok := s.Get(kept); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("after the restart: %+v, %v", got, ok)
	}
	if got, ok := s.Get(other); !ok || !got.Active || got.Latched {
		t.Errorf("another namespace's state: %+v, %v", got, ok)
	}
	if _, ok := s.Get(deleted); ok {
		t.Error("the deleted state came back")
	}
}

func TestStatesKeepControlsAcrossARestart(t *testing.T) {
	b := newStoreBackend(t)
	key := types.NamespacedName{Namespace: ns, Name: "main-valve"}
	want := ControlState{"switch.on": control.State{}}
	_ = b.holds(t)
	if err := b.controls(t).Put(t.Context(), key, want); err != nil {
		t.Fatal(err)
	}
	_ = b.holds(t)
	got, ok := b.controls(t).Get(key)
	if !ok {
		t.Fatal("the state is gone")
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("got %s, want %s", g, w)
	}
}

func TestStatesIgnoreAnUnreadableState(t *testing.T) {
	b := newStoreBackend(t)
	key := types.NamespacedName{Namespace: ns, Name: "bad"}
	b.corrupt(t, key)
	if got, ok := b.holds(t).Get(key); ok {
		t.Errorf("got %+v", got)
	}
}

func TestStoredSavesDuringShutdown(t *testing.T) {
	s := newStoreBackend(t).holds(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	key := types.NamespacedName{Namespace: ns, Name: "x"}
	if err := s.Put(ctx, key, HoldState{Active: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, ok := s.Get(key); !ok || !got.Active {
		t.Fatalf("got %+v, %v", got, ok)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(key); ok {
		t.Fatal("the deleted state is still there")
	}
}
