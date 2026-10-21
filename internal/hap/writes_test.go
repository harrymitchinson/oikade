package hap

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

var (
	kitchen    = types.NamespacedName{Namespace: "home", Name: "kitchen"}
	ceilingFan = types.NamespacedName{Namespace: "home", Name: "ceiling-fan"}
)

// failingBus fails every publish.
type failingBus struct{ mqtt.Bus }

func (failingBus) Publish(context.Context, string, []byte, bool) error {
	return errors.New("broker unreachable")
}

func TestAnnouncerAnnouncesBeforeWriting(t *testing.T) {
	bus := mqtt.NewMemory()
	var announced []string
	a := &Announcer{Bus: bus, Bridge: kitchen, Setter: driver.SetterFunc(func(_ context.Context, dev types.NamespacedName, c string, v any) error {
		for _, m := range bus.Published {
			announced = append(announced, m.Topic+" "+string(m.Payload))
		}
		if dev != ceilingFan || c != capability.SwitchOn || v != true {
			t.Errorf("Set(%v, %s, %v)", dev, c, v)
		}
		return nil
	})}
	if err := a.Set(t.Context(), ceilingFan, capability.SwitchOn, true); err != nil {
		t.Fatal(err)
	}
	want := `oikade/v1/_homekit/home/kitchen {"bridge":"kitchen","device":"ceiling-fan","capability":"switch.on","value":true}`
	if fmt.Sprint(announced) != "["+want+"]" {
		t.Errorf("announced before the write: %v, want %s", announced, want)
	}
	if bus.Published[0].Retained {
		t.Error("the announcement is retained")
	}
}

func TestAnnouncerWritesWhenItCantAnnounce(t *testing.T) {
	wrote := false
	a := &Announcer{Bus: failingBus{}, Bridge: kitchen, Setter: driver.SetterFunc(func(context.Context, types.NamespacedName, string, any) error {
		wrote = true
		return nil
	})}
	if err := a.Set(t.Context(), ceilingFan, capability.SwitchOn, true); err != nil || !wrote {
		t.Errorf("Set = %v, wrote %v; want the write made", err, wrote)
	}
}

// With no runtime on the bus, nothing hears the announcement, and the write still reaches the
// device.
func TestAnnouncerWritesWithNoRuntime(t *testing.T) {
	url := mqtttest.StartBroker(t)
	ctx := t.Context()
	hapBus, err := mqtt.Connect(ctx, url, "hap")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := mqtt.Connect(ctx, url, "plugin")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	if _, err := plugin.Subscribe(ctx, "oikade/v1/home/lan/fan-1/set", func(m mqtt.Message) { got <- string(m.Payload) }); err != nil {
		t.Fatal(err)
	}
	a := &Announcer{Bus: hapBus, Bridge: kitchen, Setter: driver.SetterFunc(func(ctx context.Context, _ types.NamespacedName, c string, v any) error {
		return hapBus.Publish(ctx, "oikade/v1/home/lan/fan-1/set", fmt.Appendf(nil, `{%q:%v}`, c, v), false)
	})}
	if err := a.Set(ctx, ceilingFan, capability.SwitchOn, true); err != nil {
		t.Fatal(err)
	}
	if p := <-got; p != `{"switch.on":true}` {
		t.Errorf("device got %s", p)
	}
}

func TestSubscribeWrites(t *testing.T) {
	bus := mqtt.NewMemory()
	var got []string
	if _, err := SubscribeWrites(t.Context(), bus, func(dev types.NamespacedName, c string, v any) {
		got = append(got, fmt.Sprintf("%s %s %v", dev, c, v))
	}); err != nil {
		t.Fatal(err)
	}
	a := &Announcer{Bus: bus, Bridge: kitchen, Setter: driver.SetterFunc(func(context.Context, types.NamespacedName, string, any) error { return nil })}
	for _, v := range []any{true, 40.0} {
		name := capability.SwitchOn
		if v == 40.0 {
			name = capability.LightLevel
		}
		if err := a.Set(t.Context(), ceilingFan, name, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{
		`not json`,
		`{"bridge":"hall","device":"ceiling-fan","capability":"switch.on","value":true}`,
		`{"bridge":"kitchen","capability":"switch.on","value":true}`,
		`{"bridge":"kitchen","device":"ceiling-fan","capability":"switch.on","value":1}`,
		`{"bridge":"kitchen","device":"ceiling-fan","capability":"no.such","value":true}`,
	} {
		_ = bus.Publish(t.Context(), WritesRoot+"home/kitchen", []byte(bad), false)
	}
	want := "[home/ceiling-fan switch.on true home/ceiling-fan light.level 40]"
	if fmt.Sprint(got) != want {
		t.Errorf("writes = %v, want %s", got, want)
	}
}

func TestAnnouncerAnnouncesArbitratedWritesOnly(t *testing.T) {
	porchLight := types.NamespacedName{Namespace: "home", Name: "porch-light"}
	store := hub.NewStore(logr.Discard())
	z2m := driver.InstanceKey{Driver: driver.Zigbee2MQTT, NamespacedName: types.NamespacedName{Namespace: "home", Name: "z2m"}}
	store.SetDevices(z2m, map[types.NamespacedName]hub.Meta{
		ceilingFan: {Instance: z2m, Control: []homev1alpha1.CapabilityControl{{
			Capability: capability.SwitchOn, Default: apiextensionsv1.JSON{Raw: []byte("false")},
		}}},
		porchLight: {Instance: z2m, Control: []homev1alpha1.CapabilityControl{{
			Capability: capability.LightOn, Default: apiextensionsv1.JSON{Raw: []byte("false")},
		}}},
	})
	store.Availability(porchLight, false)
	bus := mqtt.NewMemory()
	writes := 0
	a := &Announcer{Bus: bus, Store: store, Bridge: kitchen, Setter: driver.SetterFunc(func(context.Context, types.NamespacedName, string, any) error {
		writes++
		return nil
	})}
	for _, w := range []struct {
		dev   types.NamespacedName
		name  string
		value any
	}{
		{ceilingFan, capability.SwitchOn, true},                                            // announced
		{ceilingFan, capability.LightLevel, 40.0},                                          // not arbitrated
		{ceilingFan, capability.SwitchOn, 1.0},                                             // invalid
		{porchLight, capability.LightOn, true},                                             // offline
		{types.NamespacedName{Namespace: "home", Name: "gone"}, capability.SwitchOn, true}, // undeclared
	} {
		_ = a.Set(t.Context(), w.dev, w.name, w.value)
	}
	if len(bus.Published) != 1 || writes != 5 {
		t.Errorf("announced %v and wrote %d times, want the fan's switch.on announced and every write made", bus.Published, writes)
	}
}
