package zigbee2mqtt

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// FuzzHandle feeds Instance.handle a device list and then one message, both from the broker and so
// untrusted: whatever they hold, handle doesn't panic and leaves the Instance unlocked. The seeds
// are the fixtures' devices and state payloads, and testdata/fuzz/FuzzHandle holds the inputs that
// once broke it, which plain go test replays.
func FuzzHandle(f *testing.F) {
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range paths {
		fx, err := readFixture(path)
		if err != nil {
			f.Fatal(err)
		}
		list, err := json.Marshal([]json.RawMessage{fx.Device})
		if err != nil {
			f.Fatal(err)
		}
		for _, payload := range fx.States {
			f.Add(list, fixtureIEEE, []byte(payload))
		}
		for _, payload := range fx.Commands {
			f.Add(list, fixtureIEEE+"/set", []byte(payload))
		}
		f.Add(list, fixtureIEEE+"/availability", []byte(`{"state":"offline"}`))
		f.Add(list, "bridge/devices", []byte(`[]`))
	}

	f.Fuzz(func(t *testing.T, devices []byte, topic string, payload []byte) {
		now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		i := &Instance{
			Key: dev(DefaultBaseTopic), BaseTopic: DefaultBaseTopic, Sink: newRecorder(),
			Now: func() time.Time { return now }, Log: logging.Discard(),
		}
		// Not started, so a device list sends no /get.
		i.SetDevices(map[string]ident.ID{fixtureIEEE: dev("device")})
		for _, m := range []mqtt.Message{
			{Topic: DefaultBaseTopic + "/bridge/devices", Payload: devices},
			{Topic: DefaultBaseTopic + "/" + topic, Payload: payload},
			// Twice, since a retained message comes again on every restart.
			{Topic: DefaultBaseTopic + "/" + topic, Payload: payload},
		} {
			i.handle(m)
			if !i.mu.TryLock() {
				t.Fatalf("handle left the Instance locked after %s", m.Topic)
			}
			i.mu.Unlock()
		}
	})
}
