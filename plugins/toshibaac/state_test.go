package toshibaac

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// hexState is a state as the cloud sends it, with the fields the plugin reads and the rest as
// another unit might have them. A none field is 0xff, or 0xf for merit A.
func hexState(status, mode byte, setpoint int8, meritA byte, indoor int8) string {
	// status, mode, setpoint, fan, swing, power, merit B and A (half bytes), ion, indoor, outdoor,
	// four unknown, self-cleaning, four unknown.
	return fmt.Sprintf("%02x%02x%02x413164%x%x10%02x0affffffff10ffffffff",
		status, mode, uint8(setpoint), 0, meritA&0x0F, uint8(indoor))
}

// partial is a state that reports nothing but the fields given, as a change does.
func partial(status, mode byte, setpoint int8) string {
	return fmt.Sprintf("%02x%02x%02xffffffffffffffffffffffffffffffffffff", status, mode, uint8(setpoint))[:stateLength]
}

func TestAStateDecodes(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  contractv1.State
	}{
		{"on and heating", hexState(0x30, 0x43, 22, 0, 20), contractv1.State{"on": true, "mode": "heat", "setpoint": 22.0, "temperature": 20.0}},
		{"off, keeping its mode", hexState(0x31, 0x42, 25, 0, 27), contractv1.State{"on": false, "mode": "cool", "setpoint": 25.0, "temperature": 27.0}},
		{"dry", hexState(0x30, 0x44, 24, 0, 23), contractv1.State{"on": true, "mode": "dry", "setpoint": 24.0, "temperature": 23.0}},
		{"fan", hexState(0x30, 0x45, 24, 0, 23), contractv1.State{"on": true, "mode": "fan", "setpoint": 24.0, "temperature": 23.0}},
		{"auto", hexState(0x30, 0x41, 24, 0, 23), contractv1.State{"on": true, "mode": "auto", "setpoint": 24.0, "temperature": 23.0}},
		// 8 °C heating reports its setpoint 16 higher than it is.
		{"8 °C heating", hexState(0x30, 0x43, 24, meritHeating8C, 9), contractv1.State{"on": true, "mode": "heat", "setpoint": 8.0, "temperature": 9.0}},
		// Only while heating.
		{"8 °C merit while cooling", hexState(0x30, 0x42, 24, meritHeating8C, 9), contractv1.State{"on": true, "mode": "cool", "setpoint": 24.0, "temperature": 9.0}},
		{"a temperature it can't read", hexState(0x30, 0x43, 127, 0, -128), contractv1.State{"on": true, "mode": "heat"}},
		{"126 is -1 °C", hexState(0x30, 0x43, 22, 0, 126), contractv1.State{"on": true, "mode": "heat", "setpoint": 22.0, "temperature": -1.0}},
		{"below freezing", hexState(0x30, 0x43, 22, 0, -5), contractv1.State{"on": true, "mode": "heat", "setpoint": 22.0, "temperature": -5.0}},
		{"a status it doesn't report", hexState(0x02, 0x43, 22, 0, 20), contractv1.State{"mode": "heat", "setpoint": 22.0, "temperature": 20.0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newFCUState()
			if err := s.update(tt.state); err != nil {
				t.Fatal(err)
			}
			got, unknown := s.values()
			if !reflect.DeepEqual(got, tt.want) || unknown != 0 {
				t.Errorf("values %v (unknown mode %#x), want %v", got, unknown, tt.want)
			}
		})
	}
}

func TestAChangeKeepsWhatItDoesntReport(t *testing.T) {
	s := newFCUState()
	if err := s.update(hexState(0x30, 0x43, 22, 0, 20)); err != nil {
		t.Fatal(err)
	}
	// Someone switches to cooling at 25 in the app: the change has nothing else.
	if err := s.update(partial(0xff, 0x42, 25)); err != nil {
		t.Fatal(err)
	}
	want := contractv1.State{"on": true, "mode": "cool", "setpoint": 25.0, "temperature": 20.0}
	if got, _ := s.values(); !reflect.DeepEqual(got, want) {
		t.Errorf("values %v, want %v", got, want)
	}
	if err := s.heartbeat("15"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.values(); got["temperature"] != 21.0 {
		t.Errorf("temperature after a heartbeat of 0x15: %v", got["temperature"])
	}
}

func TestAModeItDoesntKnowIsntReported(t *testing.T) {
	s := newFCUState()
	if err := s.update(hexState(0x30, 0x47, 22, 0, 20)); err != nil {
		t.Fatal(err)
	}
	got, unknown := s.values()
	if _, has := got["mode"]; has || unknown != 0x47 {
		t.Errorf("values %v, unknown %#x", got, unknown)
	}
	// No mode at all isn't unknown.
	s = newFCUState()
	if err := s.update(hexState(0x30, 0x00, 22, 0, 20)); err != nil {
		t.Fatal(err)
	}
	if _, unknown := s.values(); unknown != 0 {
		t.Errorf("mode 0 is unknown %#x", unknown)
	}
}

func TestAStateItCantReadChangesNothing(t *testing.T) {
	for _, bad := range []string{"", "3043", "zz" + hexState(0x30, 0x43, 22, 0, 20)[2:]} {
		s := newFCUState()
		if err := s.update(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
		if got, _ := s.values(); len(got) != 0 {
			t.Errorf("%q: values %v", bad, got)
		}
	}
	s := newFCUState()
	for _, bad := range []string{"", "1", "zz", "1516"} {
		if err := s.heartbeat(bad); err == nil {
			t.Errorf("heartbeat %q: no error", bad)
		}
	}
}

func TestALongerStateDecodes(t *testing.T) {
	s := newFCUState()
	if err := s.update(hexState(0x30, 0x43, 22, 0, 20) + "0000ff"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.values(); got["setpoint"] != 22.0 {
		t.Errorf("values %v", got)
	}
}

func TestACommandEncodesAsTheAppSendsIt(t *testing.T) {
	// The library's ToshibaAcFcuState() with only ac_status = ON, encoded.
	c := newCommand()
	c.status = statusOn
	if got, want := c.encode(), "30"+strings.Repeat("f", 36); got != want {
		t.Errorf("encode() = %s, want %s", got, want)
	}
	// And it reads back as the change it is.
	c = newCommand()
	c.mode, c.setpoint, c.meritA, c.meritB, c.selfCleaning = 0x42, 25, meritOff, meritOff, selfCleaningOff
	s := newFCUState()
	if err := s.update(c.encode()); err != nil {
		t.Fatal(err)
	}
	if s.mode != 0x42 || s.setpoint != 25 || s.meritA != meritOff || s.meritB != meritOff || s.selfCleaning != selfCleaningOff || s.status != noneByte {
		t.Errorf("read back %+v", s)
	}
}

func TestACommandSetsOnlyWhatChanges(t *testing.T) {
	all := unitModes("")
	heating := func(meritA byte) fcuState {
		s := newFCUState()
		if err := s.update(hexState(0x30, 0x43, 24, meritA, 9)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cleaning := newFCUState()
	if err := cleaning.update(hexState(0x31, 0x42, 24, 0, 22)[:28] + "18" + "ffffffff"); err != nil {
		t.Fatal(err)
	}
	fireplace := heating(0)
	fireplace.meritB = 0x02

	tests := []struct {
		name       string
		state      fcuState
		capability string
		value      any
		want       func(c *command)
	}{
		{"on", heating(0), "on", true, func(c *command) { c.status = statusOn }},
		{"off", heating(0), "on", false, func(c *command) { c.status = statusOff }},
		{"on while cleaning itself", cleaning, "on", true, func(c *command) { c.status, c.selfCleaning = statusOn, selfCleaningOff }},
		{"cool", heating(0), "mode", "cool", func(c *command) { c.mode = 0x42 }},
		{"a setpoint", heating(0), "setpoint", 22.0, func(c *command) { c.setpoint = 22 }},
		// 8 °C heating takes the setpoint 16 higher, as it reports it.
		{"a setpoint in 8 °C heating", heating(meritHeating8C), "setpoint", 8.0, func(c *command) { c.setpoint = 24 }},
		// Leaving heating turns off what only heating allows.
		{"cool from 8 °C heating", heating(meritHeating8C), "mode", "cool", func(c *command) { c.mode, c.meritA = 0x42, meritOff }},
		{"fan from a fireplace mode", fireplace, "mode", "fan", func(c *command) { c.mode, c.meritB = 0x45, meritOff }},
		{"dry from eco", heating(0x03), "mode", "dry", func(c *command) { c.mode, c.meritA = 0x44, meritOff }},
		{"auto keeps eco", heating(0x03), "mode", "auto", func(c *command) { c.mode = 0x41 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.state.command(tt.capability, tt.value, all)
			if err != nil {
				t.Fatal(err)
			}
			want := newCommand()
			tt.want(&want)
			if got != want {
				t.Errorf("command %+v, want %+v", got, want)
			}
		})
	}
}

func TestACommandTheUnitCantHoldIsRefused(t *testing.T) {
	s := newFCUState()
	if err := s.update(hexState(0x30, 0x43, 24, 0, 20)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		capability string
		value      any
		modes      []byte
		says       string
	}{
		{"half a degree", "setpoint", 21.5, unitModes(""), "whole degrees"},
		{"a setpoint it can't hold", "setpoint", 126.0, unitModes(""), "can't take"},
		{"below zero", "setpoint", -2.0, unitModes(""), "can't take"},
		{"cooling on a heater", "mode", "cool", unitModes("0100"), "doesn't do cool: it does heat"},
		{"heating without a heat pump", "mode", "heat", unitModes("0200"), "doesn't do heat"},
		{"a mode there isn't", "mode", "turbo", unitModes(""), "doesn't do turbo"},
		{"on as a number", "on", 1.0, unitModes(""), "bool"},
		{"mode as a bool", "mode", true, unitModes(""), "name"},
		{"setpoint as a name", "setpoint", "warm", unitModes(""), "number"},
		{"a capability it doesn't have", "brightness", 50.0, unitModes(""), "no brightness"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.command(tt.capability, tt.value, tt.modes)
			if err == nil || !strings.Contains(err.Error(), tt.says) {
				t.Errorf("error %v, want one saying %q", err, tt.says)
			}
		})
	}
}

func TestAUnitsModesComeFromItsMeritFeature(t *testing.T) {
	tests := map[string][]byte{
		"":     {0x41, 0x42, 0x43, 0x44, 0x45},
		"0000": {0x41, 0x42, 0x43, 0x44, 0x45},
		"0300": {0x41, 0x42, 0x43, 0x44, 0x45},
		"0100": {0x43},
		"0200": {0x41, 0x42, 0x44, 0x45},
		// Short ones are padded on the right, long ones cut, and one that isn't hex does them all.
		"01":     {0x43},
		"02FFFF": {0x41, 0x42, 0x44, 0x45},
		"zz":     {0x41, 0x42, 0x43, 0x44, 0x45},
	}
	for merit, want := range tests {
		if got := unitModes(merit); !reflect.DeepEqual(got, want) {
			t.Errorf("unitModes(%q) = %x, want %x", merit, got, want)
		}
	}
}

func TestWhichModesClashOnOneOutdoorUnit(t *testing.T) {
	const auto, cool, heat, dry, fan, unknown = 0x41, 0x42, 0x43, 0x44, 0x45, 0x47
	tests := []struct {
		a, b byte
		want bool
	}{
		{heat, heat, false}, {cool, cool, false}, {auto, auto, false},
		{cool, dry, false}, {cool, fan, false}, {dry, fan, false},
		{heat, cool, true}, {heat, dry, true}, {heat, fan, true},
		{auto, cool, true}, {auto, heat, true}, {auto, fan, true},
		{unknown, heat, false}, {noneByte, cool, false},
	}
	for _, tt := range tests {
		for _, pair := range [][2]byte{{tt.a, tt.b}, {tt.b, tt.a}} {
			if got := clashes(pair[0], pair[1]); got != tt.want {
				t.Errorf("clashes(%#x, %#x) = %v, want %v", pair[0], pair[1], got, tt.want)
			}
		}
	}
}
