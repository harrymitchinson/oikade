package hue

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzEventStream reads a stream as the plugin does, and applies every event to a home read from the
// fixture: nothing a bridge sends may panic, and every device stays readable.
func FuzzEventStream(f *testing.F) {
	f.Add([]byte(": hi\n\nid: 1:0\ndata: [{\"type\":\"update\",\"data\":[{\"id\":\"027ae662-aaac-4595-acc6-03bb15e293cf\",\"type\":\"light\",\"dimming\":{\"brightness\":49.8}}]}]\n\n"))
	f.Add([]byte("data: [{\"type\":\"update\",\"data\":[{\"id\":\"bf215aa3-7c36-46c6-bbd9-7e3eda524b86\",\"type\":\"motion\",\"motion\":null}]}]\r\n\r\n"))
	f.Add([]byte("data: [{\"type\":\"update\",\"data\":[{\"id\":\"7e7e98d0-283d-4208-bbb4-c748367bce1e\",\"type\":\"device_power\",\"power_state\":{\"battery_level\":null}}]}]\n\n"))
	f.Add([]byte("data: [{\"type\":\"add\",\"data\":[{\"id\":\"x\",\"type\":\"device\"}]}]\n\ndata: [\n"))
	raw := fixture(f)
	f.Fuzz(func(t *testing.T, stream []byte) {
		h, _, err := build(raw)
		if err != nil {
			t.Fatal(err)
		}
		r := newEventReader(bytes.NewReader(stream))
		for {
			data, err := r.next()
			if err != nil {
				break
			}
			updates, err := parseEvents(data)
			if err != nil {
				continue
			}
			for _, u := range updates {
				for _, item := range u.Data {
					_, _ = h.apply(item)
				}
			}
		}
		for _, d := range h.devices {
			_ = h.online(d)
			if _, err := json.Marshal(h.values(d)); err != nil {
				t.Errorf("%s's values don't encode: %v", d.id, err)
			}
		}
	})
}
