package hue

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"oikade.dev/contract/v1"
)

func fixture(t testing.TB) []json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return a.Data
}

func TestMACID(t *testing.T) {
	for mac, want := range map[string]string{
		"7c:c6:b6:ff:fe:cb:18:14": "0x7cc6b6fffecb1814",
		"00:17:88:01:0B:2C:3D:4E": "0x001788010b2c3d4e",
		"7c:c6:b6:cb:18:14":       "",
		"7cc6b6fffecb1814":        "",
		"7c:c6:b6:ff:fe:cb:18:1g": "",
		"":                        "",
	} {
		got, ok := macID(mac)
		if got != want || ok != (want != "") {
			t.Errorf("macID(%q) = %q, %v, want %q", mac, got, ok, want)
		}
	}
}

func TestASensorLeavesOutWhatItHasntGot(t *testing.T) {
	h, _, err := build(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	sensor := h.devices["0x001788010c112233"]
	for _, change := range []string{
		`{"id":"bf215aa3-7c36-46c6-bbd9-7e3eda524b86","type":"motion","enabled":false}`,
		`{"id":"863f199f-2725-4c4b-b0ab-ee59eedcdfe2","type":"temperature","temperature":{"temperature_valid":false}}`,
	} {
		if d, err := h.apply(json.RawMessage(change)); err != nil || d != sensor {
			t.Fatalf("apply(%s) = %v, %v, want the sensor", change, d, err)
		}
	}
	// Turned off, it detects nothing.
	if got, want := h.values(sensor), (contract.State{contract.Battery: 100.0, contract.Occupied: false}); !reflect.DeepEqual(got, want) {
		t.Errorf("values %v, want %v", got, want)
	}
}

func TestAMotionWithoutAReportReadsItsOwnValue(t *testing.T) {
	h, _, err := build(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	r := h.resources["bf215aa3-7c36-46c6-bbd9-7e3eda524b86"]
	r.Motion.Report = nil
	r.Motion.Motion = true
	if got := h.values(h.devices["0x001788010c112233"])[contract.Occupied]; got != true {
		t.Errorf("occupied %v, want true", got)
	}
}

func TestADeviceThePluginCantNameIsSkipped(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"id":"d1","type":"device","metadata":{"name":"Two lights"},"product_data":{"model_id":"LCX001"},` +
			`"services":[{"rid":"l1","rtype":"light"},{"rid":"l2","rtype":"light"},{"rid":"z1","rtype":"zigbee_connectivity"}]}`),
		json.RawMessage(`{"id":"l1","type":"light","on":{"on":true}}`),
		json.RawMessage(`{"id":"l2","type":"light","on":{"on":true}}`),
		json.RawMessage(`{"id":"z1","type":"zigbee_connectivity","status":"connected","mac_address":"00:17:88:01:00:00:00:01"}`),
		json.RawMessage(`{"id":"d2","type":"device","metadata":{"name":"No MAC"},"services":[{"rid":"l3","rtype":"light"}]}`),
		json.RawMessage(`{"id":"l3","type":"light","on":{"on":true}}`),
		json.RawMessage(`{"id":"d3","type":"device","metadata":{"name":"Bad MAC"},"services":[{"rid":"l4","rtype":"light"},{"rid":"z4","rtype":"zigbee_connectivity"}]}`),
		json.RawMessage(`{"id":"l4","type":"light","on":{"on":true}}`),
		json.RawMessage(`{"id":"z4","type":"zigbee_connectivity","status":"connected","mac_address":"nope"}`),
		// A service the device lists that the bridge doesn't have counts for nothing.
		json.RawMessage(`{"id":"d4","type":"device","metadata":{"name":"Gone light"},"services":[{"rid":"l9","rtype":"light"},{"rid":"z5","rtype":"zigbee_connectivity"}]}`),
		json.RawMessage(`{"id":"z5","type":"zigbee_connectivity","status":"connected","mac_address":"00:17:88:01:00:00:00:05"}`),
	}
	h, skipped, err := build(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.devices) != 0 {
		t.Errorf("devices %v, want none", h.devices)
	}
	want := []skip{
		{model: "LCX001", name: "Two lights", reason: "it has 2 lights"},
		{name: "No MAC", reason: "it has no Zigbee MAC"},
		{name: "Bad MAC", reason: `its Zigbee MAC "nope" isn't one`},
		{name: "Gone light", reason: "it has no light or motion sensor"},
	}
	if !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped %+v\nwant %+v", skipped, want)
	}
}

func TestTwoDevicesWithOneMACKeepTheFirst(t *testing.T) {
	device := func(id, light, zigbee string) json.RawMessage {
		return json.RawMessage(`{"id":"` + id + `","type":"device","services":[{"rid":"` + light + `","rtype":"light"},{"rid":"` + zigbee + `","rtype":"zigbee_connectivity"}]}`)
	}
	raw := []json.RawMessage{
		device("a", "la", "za"), device("b", "lb", "zb"),
		json.RawMessage(`{"id":"la","type":"light","on":{"on":true}}`),
		json.RawMessage(`{"id":"lb","type":"light","on":{"on":false}}`),
		json.RawMessage(`{"id":"za","type":"zigbee_connectivity","mac_address":"00:17:88:01:00:00:00:01"}`),
		json.RawMessage(`{"id":"zb","type":"zigbee_connectivity","mac_address":"00:17:88:01:00:00:00:01"}`),
	}
	h, skipped, err := build(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d := h.devices["0x0017880100000001"]; d == nil || d.hue != "a" {
		t.Errorf("devices %v, want a's", h.devices)
	}
	if len(skipped) != 1 || skipped[0].reason != "it has the same MAC as a" {
		t.Errorf("skipped %+v", skipped)
	}
}

func TestAChangeThatCantBeReadLeavesTheResourceAsItWas(t *testing.T) {
	h, _, err := build(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.apply(json.RawMessage(`{"id":"027ae662-aaac-4595-acc6-03bb15e293cf","type":"light","dimming":{"brightness":5},"on":{"on":"yes"}}`)); err == nil {
		t.Fatal("applied a change that can't be read")
	}
	if got := h.resources["027ae662-aaac-4595-acc6-03bb15e293cf"].Dimming.Brightness; got != 100 {
		t.Errorf("brightness %v, want 100 still", got)
	}
}
