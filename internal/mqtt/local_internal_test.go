package mqtt

import (
	"net/url"
	"testing"
)

func TestFindsLocalOnlyWhatItCanDial(t *testing.T) {
	for raw, want := range map[string]bool{
		"mqtt://broker.local:1883":  true,
		"MQTTS://broker.local:8883": true,
		"tcp://broker.local:1883":   true,
		// autopaho dials WebSockets itself, through the system's resolver.
		"ws://broker.local:9001":   false,
		"wss://broker.local:8884":  false,
		"mqtt://broker.home:1883":  false,
		"mqtt://192.168.1.40:1883": false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := findsLocal(u); got != want {
			t.Errorf("findsLocal(%s) = %v, want %v", raw, got, want)
		}
	}
}
