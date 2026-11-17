package clusters

import (
	"math"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// The HVAC clusters.
const (
	IDThermostat uint32 = 0x0201
)

// DeviceThermostat is the Thermostat device type, at Matter 1.3's revision. It needs Identify, which
// every bridged endpoint has, and Thermostat. Groups is only for a device that starts things itself,
// and Thermostat Mode only arrives, optional, in 1.7.
var DeviceThermostat = im.DeviceType{ID: 0x0301, Revision: 3}

// The SystemMode values a heater uses (SystemModeEnum).
const (
	SystemModeOff  = 0
	SystemModeHeat = 4
)

// The Thermostat attributes.
const (
	attrLocalTemperature           = 0x0000
	attrOccupiedHeatingSetpoint    = 0x0012
	attrControlSequenceOfOperation = 0x001B
	attrSystemMode                 = 0x001C
)

// controlHeatingOnly is ControlSequenceOfOperation's HeatingOnly.
const controlHeatingOnly = 2

// The heating setpoint limits, in hundredths of a degree Celsius: what a radiator or a room
// thermostat can usually be set to.
const (
	MinHeatSetpoint = 500
	MaxHeatSetpoint = 3500
)

// Heater is a Thermostat cluster with only the Heating feature, for a heater or a heat-only
// thermostat. Its temperatures are in hundredths of a degree Celsius, as Matter's are.
type Heater struct {
	cluster *im.Cluster
	local   *int64
	// setpoint is OccupiedHeatingSetpoint, which isn't nullable: the spec's default until the
	// device reports.
	setpoint int64
	on       bool
	// switchable is false for a heater with no on to turn off: its SystemMode is always Heat.
	switchable bool
	// OnSetpoint is called, under the Node's lock, when a client sets the heating setpoint, in
	// degrees Celsius. It must not block.
	OnSetpoint func(celsius float64)
	// OnMode is called, under the Node's lock, when a client sets SystemMode to Off or Heat on a
	// heater that can be turned off. It must not block.
	OnMode func(heat bool)
}

// NewHeater returns a Thermostat cluster for a heater. switchable says it can be turned off, so that
// SystemMode can be Off as well as Heat.
func NewHeater(switchable bool) *Heater {
	h := &Heater{setpoint: 2000, switchable: switchable, on: !switchable}
	const featureHeating = 1 << 0
	h.cluster = &im.Cluster{
		ID: IDThermostat, Revision: 6, FeatureMap: featureHeating,
		Attributes: []im.Attribute{
			readOnly(attrLocalTemperature, func(w *tlv.Writer, tag tlv.Tag) {
				if h.local == nil {
					w.PutNull(tag)
				} else {
					w.PutInt(tag, *h.local)
				}
			}),
			intAttr(0x0003, MinHeatSetpoint), // AbsMinHeatSetpointLimit
			intAttr(0x0004, MaxHeatSetpoint), // AbsMaxHeatSetpointLimit
			{
				ID:   attrOccupiedHeatingSetpoint,
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutInt(tag, h.setpoint); return nil },
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Int()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					if v < MinHeatSetpoint || v > MaxHeatSetpoint {
						return im.StatusConstraintError
					}
					h.command(v)
					return im.StatusSuccess
				},
			},
			intAttr(0x0015, MinHeatSetpoint), // MinHeatSetpointLimit
			intAttr(0x0016, MaxHeatSetpoint), // MaxHeatSetpointLimit
			// A heater only heats: writing what it already is succeeds, and anything else can't be.
			fixed(attrControlSequenceOfOperation, controlHeatingOnly, false),
			{
				ID: attrSystemMode,
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error {
					w.PutUint(tag, h.systemMode())
					return nil
				},
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Uint()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					switch {
					case v == SystemModeHeat, v == SystemModeOff && h.switchable:
						h.mode(v == SystemModeHeat)
						return im.StatusSuccess
					}
					return im.StatusConstraintError
				},
				WritePrivilege: im.PrivilegeManage,
			},
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: h.raiseLower}, // SetpointRaiseLower
		},
	}
	return h
}

// Cluster returns the cluster to add to an endpoint.
func (h *Heater) Cluster() *im.Cluster { return h.cluster }

// SetLocal records the temperature the heater measures, in degrees Celsius, within what
// LocalTemperature can hold. Call it inside Node.Update.
func (h *Heater) SetLocal(celsius float64) {
	v := min(max(hundredths(celsius), minLocal), math.MaxInt16)
	if h.local == nil || *h.local != v {
		h.local = &v
		h.cluster.Changed(attrLocalTemperature)
	}
}

// SetSetpoint records the heater's setpoint, in degrees Celsius, as the device reports it. A
// setpoint outside the limits shows as the nearest limit, which is all a controller may be told.
// Call it inside Node.Update.
func (h *Heater) SetSetpoint(celsius float64) {
	h.set(clampSetpoint(hundredths(celsius)))
}

// SetOn records whether the heater is on: SystemMode Heat or Off. Call it inside Node.Update.
func (h *Heater) SetOn(on bool) {
	if h.switchable && on != h.on {
		h.on = on
		h.cluster.Changed(attrSystemMode)
	}
}

func (h *Heater) systemMode() uint64 {
	if h.on {
		return SystemModeHeat
	}
	return SystemModeOff
}

func (h *Heater) set(v int64) {
	if v != h.setpoint {
		h.setpoint = v
		h.cluster.Changed(attrOccupiedHeatingSetpoint)
	}
}

func (h *Heater) command(v int64) {
	h.set(v)
	if h.OnSetpoint != nil {
		h.OnSetpoint(float64(v) / 100)
	}
}

func (h *Heater) mode(heat bool) {
	h.SetOn(heat)
	if h.switchable && h.OnMode != nil {
		h.OnMode(heat)
	}
}

// raiseLower is SetpointRaiseLower: Mode Heat (0) or Both (2) moves the heating setpoint by Amount
// tenths of a degree, within the limits. A heater has no cooling setpoint for Cool (1) to move.
func (h *Heater) raiseLower(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	mode, amount, err := readRaiseLower(fields)
	if err != nil {
		return nil, err
	}
	if mode != 0 && mode != 2 {
		return nil, im.StatusConstraintError
	}
	h.command(clampSetpoint(h.setpoint + amount*10))
	return nil, nil
}

// readRaiseLower reads SetpointRaiseLower's Mode and Amount, in tenths of a degree.
func readRaiseLower(fields *tlv.Reader) (mode uint64, amount int64, err error) {
	seenMode, seenAmount := false, false
	if fields == nil {
		return 0, 0, im.StatusInvalidCommand
	}
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			mode, err = r.Uint()
			seenMode = true
		case 1:
			amount, err = r.Int()
			seenAmount = true
		}
		return err
	}); err != nil || !seenMode || !seenAmount || amount < math.MinInt8 || amount > math.MaxInt8 {
		return 0, 0, im.StatusInvalidCommand
	}
	return mode, amount, nil
}

// minLocal is the lowest LocalTemperature, absolute zero: it's an int16 of hundredths.
const minLocal = -27315

func clampSetpoint(v int64) int64 { return min(max(v, MinHeatSetpoint), MaxHeatSetpoint) }

func hundredths(celsius float64) int64 { return int64(math.Round(celsius * 100)) }
