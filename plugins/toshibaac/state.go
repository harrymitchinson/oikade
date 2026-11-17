package toshibaac

import (
	"encoding/hex"
	"fmt"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// fcuState is what the plugin keeps of an indoor unit's state, as raw bytes. The cloud sends the
// whole state as hex, in which a field it isn't reporting is a "none" value, so each report updates
// only the fields it has, as KaSroka/Toshiba-AC-control's ToshibaAcFcuState does.
type fcuState struct {
	status, mode byte
	// setpoint and indoor are signed degrees Celsius, with their own none values.
	setpoint, indoor int8
	// meritA is a half byte: the unit's feature mode, such as 8 °C heating.
	meritA byte
}

// The raw values, from KaSroka/Toshiba-AC-control's fcu_state.py.
const (
	noneByte = 0xFF
	noneHalf = 0x0F
	// noneTemp is a temperature the report doesn't have.
	noneTemp int8 = -1

	statusOn  = 0x30
	statusOff = 0x31

	meritHeating8C = 0x04
	// heating8COffset is how much higher than its real setpoint a unit in 8 °C heating reports.
	heating8COffset = 16
)

// modes are the modes a unit reports, by raw value.
var modes = map[byte]string{
	0x41: contractv1.ModeAuto,
	0x42: contractv1.ModeCool,
	0x43: contractv1.ModeHeat,
	0x44: contractv1.ModeDry,
	0x45: contractv1.ModeFan,
}

// stateLength is how many hex digits of a state the plugin reads: the merit A and B fields are a
// half byte each, and a longer state has fields the plugin doesn't need.
const stateLength = 38

// newFCUState is a state with nothing reported yet.
func newFCUState() fcuState {
	return fcuState{status: noneByte, mode: noneByte, setpoint: noneTemp, indoor: noneTemp, meritA: noneHalf}
}

// update applies a state the cloud sent, keeping every field it doesn't report.
func (s *fcuState) update(hexState string) error {
	if len(hexState) < stateLength {
		return fmt.Errorf("a state of %d hex digits, not at least %d", len(hexState), stateLength)
	}
	// The merit half bytes, at digits 12 and 13, are made whole bytes so the rest decodes in place.
	whole := hexState[:12] + "0" + hexState[12:13] + "0" + hexState[13:stateLength]
	raw, err := hex.DecodeString(whole)
	if err != nil {
		return fmt.Errorf("a state that isn't hex: %w", err)
	}
	if raw[0] != noneByte {
		s.status = raw[0]
	}
	if raw[1] != noneByte {
		s.mode = raw[1]
	}
	if t := int8(raw[2]); t != noneTemp {
		s.setpoint = t
	}
	if m := raw[7]; m != noneByte && m != noneHalf {
		s.meritA = m
	}
	if t := int8(raw[9]); t != noneTemp {
		s.indoor = t
	}
	return nil
}

// heartbeat applies a heartbeat's indoor temperature, a signed byte in hex, if it has one.
func (s *fcuState) heartbeat(iTemp string) error {
	raw, err := hex.DecodeString(iTemp)
	if err != nil || len(raw) != 1 {
		return fmt.Errorf("a heartbeat temperature %q that isn't one byte in hex", iTemp)
	}
	s.indoor = int8(raw[0])
	return nil
}

// celsius is a raw temperature in degrees, and false for one that isn't known.
func celsius(t int8) (float64, bool) {
	switch t {
	case 127, -128, noneTemp:
		return 0, false
	case 126:
		// -1 °C, since -1 is none.
		return -1, true
	}
	return float64(t), true
}

// values are the capabilities the state gives, leaving out what isn't known yet. unknownMode is a
// mode byte the plugin doesn't know, or 0.
func (s fcuState) values() (v contractv1.State, unknownMode byte) {
	v = contractv1.State{}
	switch s.status {
	case statusOn:
		v[contractv1.On] = true
	case statusOff:
		v[contractv1.On] = false
	}
	mode, known := modes[s.mode]
	switch {
	case known:
		v[contractv1.Mode] = mode
	case s.mode != noneByte && s.mode != 0x00:
		unknownMode = s.mode
	}
	if t, ok := celsius(s.setpoint); ok {
		if mode == contractv1.ModeHeat && s.meritA == meritHeating8C {
			t -= heating8COffset
		}
		v[contractv1.Setpoint] = t
	}
	if t, ok := celsius(s.indoor); ok {
		v[contractv1.Temperature] = t
	}
	return v, unknownMode
}
