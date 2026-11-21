package zigbee2mqtt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
)

// defaultInfoWait is how long the plugin waits to hear Zigbee2MQTT's bridge/info, which is retained,
// so it comes straight after subscribing if Zigbee2MQTT uses this broker.
const defaultInfoWait = 5 * time.Second

// bridgeInfo is what the plugin reads of <base>/bridge/info: only the settings setup checks. The rest
// of its configuration, which holds the network key, is never read.
type bridgeInfo struct {
	Version string `json:"version"`
	Config  struct {
		// Availability is true or false, an object with enabled from 2.0, or an object of timeouts
		// before it, which turned it on.
		Availability  json.RawMessage `json:"availability"`
		DeviceOptions struct {
			Retain json.RawMessage `json:"retain"`
		} `json:"device_options"`
		// Advanced holds 1.x's availability_timeout, which turned availability on before availability
		// had a key of its own. Nothing else of it is read.
		Advanced struct {
			AvailabilityTimeout json.RawMessage `json:"availability_timeout"`
		} `json:"advanced"`
	} `json:"config"`
	// unreadable is a bridge/info that isn't JSON of this shape, so nothing can be said of it.
	unreadable bool
}

// readInfo reads a bridge/info. One that isn't this shape is unreadable, not missing: Zigbee2MQTT is
// there.
func readInfo(payload []byte) bridgeInfo {
	var info bridgeInfo
	if json.Unmarshal(payload, &info) != nil {
		return bridgeInfo{unreadable: true}
	}
	return info
}

// problems are the settings that are wrong, each with what it costs and what to set.
func (i bridgeInfo) problems() []string {
	if i.unreadable {
		return []string{"Zigbee2MQTT's bridge/info isn't what this plugin can read, so its settings can't be checked"}
	}
	var out []string
	if !availabilityOn(i.Config.Availability) && !positive(i.Config.Advanced.AvailabilityTimeout) {
		out = append(out, "availability is off in Zigbee2MQTT: a device that's gone won't read offline. "+
			"Set availability.enabled: true")
	}
	var retain bool
	if json.Unmarshal(i.Config.DeviceOptions.Retain, &retain) != nil || !retain {
		out = append(out, "retain is off in Zigbee2MQTT: a sensor's values are unknown after a host restart "+
			"until it reports again. Set device_options.retain: true")
	}
	return out
}

func availabilityOn(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	var on bool
	if json.Unmarshal(raw, &on) == nil {
		return on
	}
	var obj struct {
		Enabled *bool `json:"enabled"`
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil {
		return false
	}
	// Before 2.0, an object of timeouts turned it on, and had no enabled.
	return obj.Enabled == nil || *obj.Enabled
}

func positive(raw json.RawMessage) bool {
	var n float64
	return json.Unmarshal(raw, &n) == nil && n > 0
}

// heardInfo takes in bridge/info, and says whether the plugin needs its setup run.
func (b *bridge) heardInfo(payload []byte) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return // a retained bridge/info cleared
	}
	info := readInfo(payload)
	b.mu.Lock()
	first := b.info == nil
	b.info = &info
	b.mu.Unlock()
	if first {
		close(b.infoHeard)
	}
	b.check(b.plugin.SetNeedsSetup(b.ctx, len(info.problems()) > 0), "Could not report whether setup is needed", "")
}

// awaitInfo says the plugin needs its setup if Zigbee2MQTT isn't heard soon after it subscribes: it
// isn't on this broker, or under another base topic.
func (b *bridge) awaitInfo(ctx context.Context) {
	select {
	case <-b.infoHeard:
	case <-time.After(b.infoWait):
		b.check(b.plugin.SetNeedsSetup(ctx, true), "Could not report whether setup is needed", "")
		// The flag goes with the devices, which have no list to come from: those on record aren't
		// found. A device list that comes later replaces this.
		b.reporting.Lock()
		defer b.reporting.Unlock()
		b.mu.Lock()
		listed := b.reported != nil
		b.mu.Unlock()
		if !listed {
			reports := make([]contract.DeviceReport, 0, len(b.declared))
			for _, id := range b.declared {
				reports = append(reports, contract.DeviceReport{ID: id})
			}
			b.check(b.plugin.SetDevices(ctx, reports, 0), "Could not report devices", "")
		}
	case <-ctx.Done():
	}
}

// stateWait is how long setup waits for bridge/state after bridge/info. A broker sends retained
// messages together, so one that isn't heard by then isn't retained.
const stateWait = 500 * time.Millisecond

// Setup implements plugin.Setup. It checks what the plugin can't fix: that it reaches the broker,
// that Zigbee2MQTT uses it and is up, and Zigbee2MQTT's settings. It changes nothing in Zigbee2MQTT,
// whose configuration is yours.
func (b *bridge) Setup(ctx context.Context, _ *plugin.Plugin, s *plugin.Session) (string, error) {
	broker := b.config.Broker
	// The plugin may be connecting still, or again.
	deadline := time.Now().Add(b.infoWait)
	for !b.isConnected() {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("could not reach the broker %s from the host: check zigbee2mqtt.Config's Broker and Password", broker)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := s.Say(ctx, fmt.Sprintf("Broker %s: connected", broker)); err != nil {
		return "", err
	}
	select {
	case <-b.infoHeard:
	case <-time.After(time.Until(deadline)):
		return "", fmt.Errorf("found no Zigbee2MQTT on %s under %s/bridge: check that it uses this broker, "+
			"and that its mqtt.base_topic is %s, or set zigbee2mqtt.Config's BaseTopic", broker, b.config.BaseTopic, b.config.BaseTopic)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// Zigbee2MQTT retains its bridge/state as it does its bridge/info, and the broker sends them
	// one after the other in either order, so the state comes soon after the info if it's there.
	// Until it does, up is only the default. The wait is stateWait however late the info came:
	// the deadline is for finding Zigbee2MQTT, which the info has done.
	select {
	case <-b.stateHeard:
	case <-time.After(stateWait):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	b.mu.Lock()
	info, up := *b.info, b.up
	paired := 0
	for _, found := range b.reported {
		if found {
			paired++
		}
	}
	b.mu.Unlock()
	if !up {
		return "", fmt.Errorf("Zigbee2MQTT %s is on the broker, but its bridge/state says it's offline: start it", info.Version) //nolint:staticcheck // a name
	}
	if err := s.Say(ctx, fmt.Sprintf("Zigbee2MQTT %s found", info.Version)); err != nil {
		return "", err
	}
	problems := info.problems()
	for _, p := range problems {
		if err := s.Say(ctx, p); err != nil {
			return "", err
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%s needs %s in Zigbee2MQTT's configuration.yaml. Run oikade setup %s again after it",
			b.name, plural(len(problems), "change"), b.name)
	}
	return fmt.Sprintf("%s is set up: %s paired", b.name, plural(paired, "device")), nil
}

func (b *bridge) isConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
