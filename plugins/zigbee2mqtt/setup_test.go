package zigbee2mqtt

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

// Written by hand from Zigbee2MQTT's documented bridge/info, with only what setup reads and a network
// key it must leave alone. Not recorded from a real Zigbee2MQTT.
const (
	infoSetUp = `{"version":"2.6.0","config":{"availability":{"enabled":true,"active":{"timeout":10}},` +
		`"device_options":{"retain":true},"advanced":{"network_key":[1,2,3]}}}`
	infoDefaults = `{"version":"2.6.0","config":{"availability":{"enabled":false},"advanced":{"network_key":[1,2,3]}}}`
	// Before 2.0, availability was true, or an object of timeouts.
	infoOldTrue     = `{"version":"1.42.0","config":{"availability":true,"device_options":{"retain":true}}}`
	infoOldTimeouts = `{"version":"1.42.0","config":{"availability":{"active":{"timeout":10}},"device_options":{"retain":true}}}`
	infoOldOff      = `{"version":"1.42.0","config":{"device_options":{"retain":false}}}`
)

func TestBridgeInfoNamesTheSettingsThatAreWrong(t *testing.T) {
	for payload, want := range map[string][]string{
		infoSetUp:       nil,
		infoOldTrue:     nil,
		infoOldTimeouts: nil,
		infoDefaults:    {"availability.enabled: true", "device_options.retain: true"},
		infoOldOff:      {"availability.enabled: true", "device_options.retain: true"},
	} {
		var info bridgeInfo
		if err := json.Unmarshal([]byte(payload), &info); err != nil {
			t.Fatal(err)
		}
		got := info.problems()
		if len(got) != len(want) {
			t.Errorf("%s: got %q, want %q", payload, got, want)
			continue
		}
		for i, w := range want {
			if !strings.HasSuffix(got[i], "Set "+w) {
				t.Errorf("%s: got %q, want it to end Set %s", payload, got[i], w)
			}
		}
	}
}

// runSetup runs the plugin's setup as the host would, and returns its steps.
func runSetup(t *testing.T, url string) []contractv1.SetupStep {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c, err := mqtt.Connect(ctx, url, "z2m-setup-"+clientID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(context.Background()) }()
	got := make(chan contractv1.SetupStep, 16)
	if _, err := c.Subscribe(ctx, root.SetupOut(), func(m mqtt.Message) {
		var s contractv1.SetupStep
		if json.Unmarshal(m.Payload, &s) == nil {
			got <- s
		}
	}); err != nil {
		t.Fatal(err)
	}
	start, _ := json.Marshal(contractv1.SetupRequest{Session: "s", Start: true})
	if err := c.Publish(ctx, root.SetupIn(), start, false); err != nil {
		t.Fatal(err)
	}
	var steps []contractv1.SetupStep
	for {
		select {
		case s := <-got:
			steps = append(steps, s)
			if s.Done != nil {
				return steps
			}
		case <-ctx.Done():
			t.Fatalf("setup didn't finish: %+v", steps)
		}
	}
}

func says(steps []contractv1.SetupStep) []string {
	var out []string
	for _, s := range steps {
		if s.Say != "" {
			out = append(out, s.Say)
		}
	}
	return out
}

func TestSetupChecksZigbee2MQTTsSettings(t *testing.T) {
	b := mqtttest.NewBroker(t)
	h := hear(t, b.URL)
	h.publish(t, base+"/bridge/info", infoDefaults, true)
	h.publish(t, base+"/bridge/devices", bridgeDevices(t), true)
	run(t, b.URL)
	h.wait(t, "needsSetup", func(topics map[string]string) bool { return reported(t, topics[root.Devices()]).NeedsSetup })

	steps := runSetup(t, b.URL)
	said := says(steps)
	if len(said) != 4 || !strings.HasPrefix(said[0], "Broker mqtt://") || said[1] != "Zigbee2MQTT 2.6.0 found" ||
		!strings.HasPrefix(said[2], "availability is off") || !strings.HasPrefix(said[3], "retain is off") {
		t.Errorf("said %q", said)
	}
	done := steps[len(steps)-1].Done
	if done.Error != "zigbee needs 2 changes in Zigbee2MQTT's configuration.yaml. Run oikade setup zigbee again after it" {
		t.Errorf("done %+v", done)
	}
	for _, s := range steps {
		if raw, _ := json.Marshal(s); strings.Contains(string(raw), "network_key") {
			t.Errorf("setup passed on the network key: %s", raw)
		}
	}

	// Fixed in Zigbee2MQTT, which republishes its info.
	h.publish(t, base+"/bridge/info", infoSetUp, true)
	h.wait(t, "needsSetup to clear", func(topics map[string]string) bool {
		d := reported(t, topics[root.Devices()])
		return d.Devices != nil && !d.NeedsSetup
	})
	steps = runSetup(t, b.URL)
	if done := steps[len(steps)-1].Done; done.Error != "" || !strings.HasPrefix(done.Result, "zigbee is set up: ") {
		t.Errorf("done %+v", done)
	}
	if said := says(steps); slices.ContainsFunc(said, func(s string) bool { return strings.Contains(s, " is off") }) {
		t.Errorf("said %q", said)
	}
}

func TestSetupSaysWhenZigbee2MQTTIsntOnTheBroker(t *testing.T) {
	b := mqtttest.NewBroker(t)
	run(t, b.URL)
	start := time.Now()
	steps := runSetup(t, b.URL)
	done := steps[len(steps)-1].Done
	if !strings.HasPrefix(done.Error, "found no Zigbee2MQTT on mqtt://") || !strings.Contains(done.Error, "z2m/bridge") {
		t.Errorf("done %+v", done)
	}
	if took := time.Since(start); took < infoWait {
		t.Errorf("gave up after %s", took)
	}
}
