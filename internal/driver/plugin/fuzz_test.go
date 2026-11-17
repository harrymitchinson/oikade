package plugin

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// FuzzHandle feeds Instance.handle a devices message and then one message on any topic, twice, as
// anything on the broker can publish them under a plugin's root. It doesn't panic, leaves the
// Instance unlocked, and its counts of rejected messages only rise.
func FuzzHandle(f *testing.F) {
	f.Add([]byte(devicesJSON), "1000aa/state", []byte(`{"on":true,"temperature":21.5}`))
	f.Add([]byte(devicesJSON), "1000aa/state", []byte(`{"on":"yes","colour.hue":3}`))
	f.Add([]byte(devicesJSON), "1000zz/state", []byte(`{"on":true}`))
	f.Add([]byte(devicesJSON), "1000bb/availability", []byte(`offline`))
	f.Add([]byte(devicesJSON), "status", []byte(`{"state":"online","contract":2}`))
	f.Add([]byte(devicesJSON), "status", []byte(`{"state":"online","contract":1,"error":"no cloud"}`))
	f.Add([]byte(devicesJSON), "devices", []byte(`{"devices":[{"id":"1000aa","found":true,"capabilities":["on"]}],"discovered":[{"id":"x"}]}`))
	f.Add([]byte(devicesJSON), "_setup/out", []byte(`{"session":"s","seq":1,"ask":{"prompt":"App ID"}}`))
	f.Add([]byte(devicesJSON), "_setup/out", []byte(`{"session":"s","seq":2,"done":{"result":"set up"}}`))
	f.Add([]byte(`{"devices":[{"id":"","found":true},{"id":"1000aa"},{"id":"1000aa"}]}`), "1000aa/state", []byte(`{}`))

	f.Fuzz(func(t *testing.T, devices []byte, topic string, payload []byte) {
		bus := mqtt.NewMemory()
		i := &Instance{
			Key: dev("sonoff"), Root: root, Bus: bus, Sink: newRecorder(),
			Now: func() time.Time { return now }, Log: logging.Discard(),
		}
		i.SetDevices(map[string]ident.ID{"1000aa": porch, "1000bb": hall, "1000ff": ghost})
		rejected := map[string]uint64{}
		send := func(topic string, payload []byte) {
			i.handle(mqtt.Message{Topic: topic, Payload: payload})
			if !i.counting.TryLock() || !i.mu.TryLock() {
				t.Fatalf("handle left the Instance locked after %s", topic)
			}
			i.mu.Unlock()
			i.counting.Unlock()
			got := i.Stats().Rejected
			for reason, n := range rejected {
				if got[reason] < n {
					t.Fatalf("after %s, %s fell from %d to %d", topic, reason, n, got[reason])
				}
			}
			rejected = got
		}
		send(root+"/status", []byte(`{"state":"online","contract":1,"setup":true}`))
		steps, err := i.StartSetup(t.Context(), "s")
		if err != nil {
			t.Fatal(err)
		}
		send(root+"/devices", devices)
		send(root+"/"+topic, payload)
		send(root+"/"+topic, payload)
		// Read what the session got, as its reader would, without waiting for more.
		for len(steps) > 0 {
			<-steps
		}
	})
}
