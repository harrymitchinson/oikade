package zigbee2mqtt

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// fromFixture starts an Instance whose only device is a fixture's.
func fromFixture(t *testing.T, name string) (*Instance, *mqtt.Memory, *recorder) {
	t.Helper()
	f, err := readFixture(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	list, err := json.Marshal([]json.RawMessage{f.Device})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bus := mqtt.NewMemory()
	_ = bus.Publish(ctx, DefaultBaseTopic+"/bridge/devices", list, true)
	rec := newRecorder()
	i := &Instance{
		Key: dev(DefaultBaseTopic), BaseTopic: DefaultBaseTopic, Bus: bus, Sink: rec,
		Now: time.Now, Log: logging.Discard(), getInterval: time.Hour,
	}
	i.SetDevices(map[string]ident.ID{fixtureIEEE: dev("device")})
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.Stop)
	return i, bus, rec
}

// lastChange is the last value reported for a capability.
func lastChange(rec *recorder, name string) any {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, u := range slices.Backward(rec.changes) {
		if u.Capability == name {
			return u.Value
		}
	}
	return nil
}

// A cover's position is served the other way round: Zigbee2MQTT's 100 is open, and Oikade's closed.
func TestACoversPositionIsReversedBothWays(t *testing.T) {
	ctx := context.Background()
	i, bus, rec := fromFixture(t, "ikea-e1757.json")
	_ = bus.Publish(ctx, DefaultBaseTopic+"/"+fixtureIEEE, []byte(`{"position":30}`), false)
	if got := lastChange(rec, capability.Position); got != 70.0 {
		t.Errorf("position 30 is %v, want 70", got)
	}
	if err := i.Set(ctx, dev("device"), capability.Position, 70.0); err != nil {
		t.Fatal(err)
	}
	if sent := bus.Sent(DefaultBaseTopic + "/" + fixtureIEEE + "/set"); !slices.Equal(sent, []string{`{"position":30}`}) {
		t.Errorf("setting 70 sent %v", sent)
	}
}

// A cover with no position only opens and closes, which position can't say, so it isn't served.
func TestACoverWithoutAPositionIsntServed(t *testing.T) {
	state := expose{Name: "state", Type: "enum", Property: "state"}
	position := expose{Name: "position", Type: exposeNumeric, Property: "position", Access: 7}
	if got := bindings([]expose{{Type: exposeCover, Features: []expose{state}}}); len(got) != 0 {
		t.Errorf("a cover without position binds %+v", got)
	}
	got := bindings([]expose{{Type: exposeCover, Features: []expose{state, position}}})
	if len(got) != 1 || got[0].capability != capability.Position || !got[0].reverse {
		t.Errorf("a cover with position binds %+v", got)
	}
}

// A lock is locked from lock_state, where the bolt is, and set with state's LOCK and UNLOCK.
func TestALockReadsLockStateAndSetsState(t *testing.T) {
	ctx := context.Background()
	i, bus, rec := fromFixture(t, "yale-yrd226ha2619.json")
	topic := DefaultBaseTopic + "/" + fixtureIEEE
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"state":"LOCK","lock_state":"locked"}`, true},
		// Told to lock, but the bolt isn't home.
		{`{"state":"LOCK","lock_state":"not_fully_locked"}`, false},
		{`{"state":"LOCK","lock_state":"locked"}`, true},
		{`{"state":"UNLOCK","lock_state":"unlocked"}`, false},
	} {
		_ = bus.Publish(ctx, topic, []byte(tc.payload), false)
		if got := lastChange(rec, capability.Locked); got != tc.want {
			t.Errorf("%s is locked %v, want %v", tc.payload, got, tc.want)
		}
	}
	for _, v := range []bool{true, false} {
		if err := i.Set(ctx, dev("device"), capability.Locked, v); err != nil {
			t.Fatal(err)
		}
	}
	if sent := bus.Sent(topic + "/set"); !slices.Equal(sent, []string{`{"state":"LOCK"}`, `{"state":"UNLOCK"}`}) {
		t.Errorf("setting locked sent %v", sent)
	}
}

// A lock with no lock_state is read from state.
func TestALockWithoutLockStateReadsState(t *testing.T) {
	state := expose{Name: "state", Type: exposeBinary, Property: "state", ValueOn: "LOCK", ValueOff: "UNLOCK", Access: 7}
	got := bindings([]expose{{Type: exposeLock, Features: []expose{state}}})
	if len(got) != 1 || got[0].property != "state" || got[0].states != nil {
		t.Fatalf("binds %+v", got)
	}
	if v, err := got[0].decode("LOCK"); err != nil || v != true {
		t.Errorf("LOCK is %v, %v", v, err)
	}
}
