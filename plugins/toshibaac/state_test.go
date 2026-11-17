package toshibaac

import (
	"fmt"
	"reflect"
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
