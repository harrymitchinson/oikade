package toshibaac

import (
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// fcuState is what the plugin keeps of an indoor unit's state, as raw bytes. The cloud sends the
// whole state as hex, in which a field it isn't reporting is a "none" value, so each report updates
// only the fields it has, as KaSroka/Toshiba-AC-control's ToshibaAcFcuState does.
type fcuState struct {
	status, mode byte
	// setpoint and indoor are signed degrees Celsius, with their own none values.
	setpoint, indoor int8
	// meritA and meritB are half bytes: the unit's feature modes, such as 8 °C heating or a
	// fireplace mode.
	meritA, meritB byte
	selfCleaning   byte
}

// The raw values, from KaSroka/Toshiba-AC-control's fcu_state.py.
const (
	noneByte = 0xFF
	noneHalf = 0x0F
	// noneTemp is a temperature the report doesn't have.
	noneTemp int8 = -1

	statusOn  = 0x30
	statusOff = 0x31

	meritOff       = 0x00
	meritHeating8C = 0x04

	selfCleaningOn  = 0x18
	selfCleaningOff = 0x10
	// heating8COffset is how much higher than its real setpoint a unit in 8 °C heating reports.
	heating8COffset = 16
)

const (
	modeAuto = 0x41
	modeHeat = 0x43
	modeFan  = 0x45
)

// modes are the modes a unit reports, by raw value.
var modes = map[byte]string{
	modeAuto: contractv1.ModeAuto,
	0x42:     contractv1.ModeCool,
	modeHeat: contractv1.ModeHeat,
	0x44:     contractv1.ModeDry,
	modeFan:  contractv1.ModeFan,
}

// stateLength is how many hex digits of a state the plugin reads: the merit A and B fields are a
// half byte each, and a longer state has fields the plugin doesn't need.
const stateLength = 38

// newFCUState is a state with nothing reported yet.
func newFCUState() fcuState {
	return fcuState{
		status: noneByte, mode: noneByte, setpoint: noneTemp, indoor: noneTemp, meritA: noneHalf, meritB: noneHalf,
		selfCleaning: noneByte,
	}
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
	if m := raw[6]; m != noneByte && m != noneHalf {
		s.meritB = m
	}
	if m := raw[7]; m != noneByte && m != noneHalf {
		s.meritA = m
	}
	if t := int8(raw[9]); t != noneTemp {
		s.indoor = t
	}
	if raw[15] != noneByte {
		s.selfCleaning = raw[15]
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
		if s.mode == modeHeat && s.meritA == meritHeating8C {
			t -= heating8COffset
		}
		v[contractv1.Setpoint] = t
	}
	if t, ok := celsius(s.indoor); ok {
		v[contractv1.Temperature] = t
	}
	return v, unknownMode
}

// allModes are the modes' raw values, in order, and modeBytes each mode's by name.
var (
	allModes  = slices.Sorted(maps.Keys(modes))
	modeBytes = func() map[string]byte {
		m := map[string]byte{}
		for b, name := range modes {
			m[name] = b
		}
		return m
	}()
)

// clashes is whether units on one outdoor unit can't run modes a and b at once, from Toshiba's
// manuals for its multi-split units: heating can't run alongside cooling or dry, which need the
// outdoor unit the other way. Fan runs beside any mode, since it doesn't use the outdoor unit
// (OIK-324). Auto clashes with heating, cooling and dry, since it may pick either. A mode the
// plugin doesn't know clashes with nothing.
func clashes(a, b byte) bool {
	_, knowA := modes[a]
	_, knowB := modes[b]
	if !knowA || !knowB || a == b || a == modeFan || b == modeFan {
		return false
	}
	return a == modeAuto || b == modeAuto || a == modeHeat || b == modeHeat
}

// The merit features a mode turns off, from KaSroka/Toshiba-AC-control's features.py: merit A's
// high power (1), silent (2, 10), eco (3), 8 °C heating (4), sleep care (5) and floor (6), and
// merit B's fireplace modes (2, 3). Heating allows them all.
var (
	meritAOff = map[byte][]byte{
		0x41: {4, 5, 6}, 0x42: {4, 5, 6},
		0x44: {1, 2, 3, 4, 5, 6, 10}, 0x45: {1, 2, 3, 4, 5, 6, 10},
	}
	meritBOff = map[byte][]byte{0x41: {2, 3}, 0x42: {2, 3}, 0x44: {2, 3}, 0x45: {2, 3}}
)

// command is a change to a unit: a state with only the fields to change, the rest none.
type command struct {
	status, mode   byte
	setpoint       int8
	meritA, meritB byte
	selfCleaning   byte
}

func newCommand() command {
	return command{status: noneByte, mode: noneByte, setpoint: noneTemp, meritA: noneByte, meritB: noneByte, selfCleaning: noneByte}
}

// command is the change that sets capability to value on a unit in state s that does the modes
// given. It refuses what the plugin knows the unit can't hold, rather than letting it round: a mode
// it doesn't do, or a setpoint that isn't whole degrees or that the protocol can't carry. Which
// setpoints a unit takes isn't known, so one it clamps is sent.
func (s fcuState) command(capability string, value any, unitModes []byte) (command, error) {
	c := newCommand()
	mode := s.mode
	switch capability {
	case contractv1.On:
		on, ok := value.(bool)
		if !ok {
			return c, fmt.Errorf("on is a bool, not %T", value)
		}
		c.status = statusOff
		if on {
			c.status = statusOn
			// A unit cleaning itself doesn't turn on until that's cleared, as the app does.
			if s.selfCleaning == selfCleaningOn {
				c.selfCleaning = selfCleaningOff
			}
		}
	case contractv1.Mode:
		name, ok := value.(string)
		if !ok {
			return c, fmt.Errorf("mode is a name, not %T", value)
		}
		b, known := modeBytes[name]
		if !known || !slices.Contains(unitModes, b) {
			return c, fmt.Errorf("the unit doesn't do %s: it does %s", name, modeNames(unitModes))
		}
		c.mode, mode = b, b
	case contractv1.Setpoint:
		t, ok := value.(float64)
		if !ok {
			return c, fmt.Errorf("setpoint is a number, not %T", value)
		}
		if t != math.Trunc(t) {
			return c, fmt.Errorf("the unit takes whole degrees, not %v", t)
		}
		raw := int(t)
		// In 8 °C heating the unit takes its setpoint 16 higher, as it reports it.
		if s.mode == modeHeat && s.meritA == meritHeating8C {
			raw += heating8COffset
		}
		// 126 and 127 aren't setpoints, and -1 is none.
		if raw < 0 || raw > 125 {
			return c, fmt.Errorf("the unit can't take a setpoint of %v", t)
		}
		c.setpoint = int8(raw)
	default:
		return c, fmt.Errorf("the unit has no %s", capability)
	}
	// A feature the mode doesn't allow is turned off with it, as the app does.
	if slices.Contains(meritAOff[mode], s.meritA) {
		c.meritA = meritOff
	}
	if slices.Contains(meritBOff[mode], s.meritB) {
		c.meritB = meritOff
	}
	return c, nil
}

// encode is the command in hex, as the cloud takes it: fan, swing, power, ion and the
// temperatures are none, and the merit fields are a half byte each.
func (c command) encode() string {
	raw := []byte{
		c.status, c.mode, byte(c.setpoint), noneByte, noneByte, noneByte, c.meritB, c.meritA, noneByte,
		noneByte, noneByte, noneByte, noneByte, noneByte, noneByte, c.selfCleaning,
		noneByte, noneByte, noneByte, noneByte,
	}
	h := hex.EncodeToString(raw)
	// Digits 12 and 14 are the merit bytes' high halves.
	return h[:12] + h[13:14] + h[15:]
}

// unitModes are the modes a unit does, from bits 6 and 7 of its MeritFeature: a heat pump does
// them all, a heater only heats, and an air conditioner without heat pump does all but heating.
// A MeritFeature the plugin can't read does them all.
func unitModes(merit string) []byte {
	// Four digits, padded or cut, as the library reads it.
	v, err := strconv.ParseUint((merit + "0000")[:4], 16, 16)
	if err != nil {
		return allModes
	}
	bit := func(i int) bool { return v>>(15-i)&1 == 1 }
	switch {
	case !bit(6) && bit(7):
		return []byte{modeHeat}
	case bit(6) && !bit(7):
		return slices.DeleteFunc(slices.Clone(allModes), func(b byte) bool { return b == modeHeat })
	}
	return allModes
}

func modeNames(bs []byte) string {
	names := make([]string, 0, len(bs))
	for _, b := range bs {
		names = append(names, modes[b])
	}
	return strings.Join(names, ", ")
}
