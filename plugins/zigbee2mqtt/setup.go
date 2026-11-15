package zigbee2mqtt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// infoWait is how long setup waits to hear Zigbee2MQTT's bridge/info, which is retained, so it comes
// straight after subscribing if Zigbee2MQTT uses this broker.
const infoWait = 5 * time.Second

// bridgeInfo is what the plugin reads of <base>/bridge/info: only the settings setup checks. The rest
// of its configuration, which holds the network key, is never read.
type bridgeInfo struct {
	Version string `json:"version"`
	Config  struct {
		// Availability is true or false, an object with enabled from 2.0, or an object of timeouts
		// before it, which turned it on.
		Availability  json.RawMessage `json:"availability"`
		DeviceOptions struct {
			Retain *bool `json:"retain"`
		} `json:"device_options"`
	} `json:"config"`
}

// problems are the settings that are wrong, each with what it costs and what to set.
func (i bridgeInfo) problems() []string {
	var out []string
	if !availabilityOn(i.Config.Availability) {
		out = append(out, "availability is off in Zigbee2MQTT: a device that's gone won't read offline. "+
			"Set availability.enabled: true")
	}
	if r := i.Config.DeviceOptions.Retain; r == nil || !*r {
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

// heardInfo takes in bridge/info, and says whether the plugin needs its setup run.
func (b *bridge) heardInfo(payload []byte) {
	var info bridgeInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		// Not the payload: it holds the network key.
		b.log.Warn("Could not read Zigbee2MQTT's bridge info", "err", err)
		return
	}
	b.mu.Lock()
	first := b.info == nil
	b.info = &info
	heard := b.infoHeardLocked()
	b.mu.Unlock()
	if first {
		close(heard)
	}
	b.check(b.plugin.SetNeedsSetup(b.ctx, len(info.problems()) > 0), "Could not report whether setup is needed", "")
}

func (b *bridge) infoHeardLocked() chan struct{} {
	if b.infoHeard == nil {
		b.infoHeard = make(chan struct{})
	}
	return b.infoHeard
}

// Setup implements plugin.Setup. It checks what the plugin can't fix: that it reaches the broker,
// that Zigbee2MQTT uses it, and Zigbee2MQTT's settings. It changes nothing in Zigbee2MQTT, whose
// configuration is yours.
func (b *bridge) Setup(ctx context.Context, _ *plugin.Plugin, s *plugin.Session) (string, error) {
	broker := b.broker
	if u, err := url.Parse(b.broker); err == nil {
		broker = u.Redacted()
	}
	b.mu.Lock()
	connected := b.connected
	heard := b.infoHeardLocked()
	b.mu.Unlock()
	if !connected {
		return "", fmt.Errorf("could not reach the broker %s from the host: check the host's --broker", broker)
	}
	if err := s.Say(ctx, fmt.Sprintf("Broker %s: connected", broker)); err != nil {
		return "", err
	}
	select {
	case <-heard:
	case <-time.After(infoWait):
		return "", fmt.Errorf("found no Zigbee2MQTT on %s under %s/bridge: check that it uses this broker, "+
			"and that its mqtt.base_topic is %s, or set zigbee2mqtt.Config's BaseTopic", broker, b.config.BaseTopic, b.config.BaseTopic)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	b.mu.Lock()
	info := *b.info
	paired := 0
	for _, found := range b.reported {
		if found {
			paired++
		}
	}
	b.mu.Unlock()
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

func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
