package clusters

import (
	"math"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// DeviceRoomAirConditioner is the Room Air Conditioner device type, at the revision matter.js
// serves. It needs Identify, which every bridged endpoint has, On/Off and Thermostat. Groups is
// only for a device that starts things itself, as for a Thermostat.
var DeviceRoomAirConditioner = im.DeviceType{ID: 0x0072, Revision: 2}

// The other SystemMode values an air conditioner uses (SystemModeEnum).
const (
	SystemModeAuto    = 1
	SystemModeCool    = 3
	SystemModeFanOnly = 7
	SystemModeDry     = 8
)

// The Thermostat attributes only an air conditioner has.
const (
	attrOccupiedCoolingSetpoint = 0x0011
	attrMinSetpointDeadBand     = 0x0019
)

// FeatureDeadFront is On/Off's DeadFrontBehavior, which a Room Air Conditioner's has: off, the
// other clusters show what they can.
const FeatureDeadFront = 1 << 1

// controlCoolingAndHeating is ControlSequenceOfOperation's CoolingAndHeating.
const controlCoolingAndHeating = 4

// AirConditioner is a Thermostat cluster with the Heating, Cooling and Auto features, for an air
// conditioner or heat pump with one target temperature: both occupied setpoints are it, so the
// dead band between them is 0. Its temperatures are in hundredths of a degree Celsius, as Matter's
// are.
type AirConditioner struct {
	cluster *im.Cluster
	local   *int64
	// setpoint is both occupied setpoints, which aren't nullable: the spec's default until the
	// device reports.
	setpoint int64
	on       bool
	// mode is the SystemMode while on: Auto, Cool, Heat, FanOnly or Dry. It's kept while off.
	mode uint64
	// OnSetpoint is called, under the Node's lock, when a client sets the setpoint of the side in
	// use, in degrees Celsius. It must not block.
	OnSetpoint func(celsius float64)
	// OnSystemMode is called, under the Node's lock, when a client sets SystemMode: Off is on
	// false, and any other is on true in that mode. It must not block.
	OnSystemMode func(on bool, mode uint64)
}

// NewAirConditioner returns a Thermostat cluster for an air conditioner, off and in Auto until the
// device reports.
func NewAirConditioner() *AirConditioner {
	a := &AirConditioner{setpoint: 2000, mode: SystemModeAuto}
	const featureHeating, featureCooling, featureAuto = 1 << 0, 1 << 1, 1 << 5
	a.cluster = &im.Cluster{
		ID: IDThermostat, Revision: 6, FeatureMap: featureHeating | featureCooling | featureAuto,
		Attributes: []im.Attribute{
			readOnly(attrLocalTemperature, func(w *tlv.Writer, tag tlv.Tag) {
				if a.local == nil {
					w.PutNull(tag)
				} else {
					w.PutInt(tag, *a.local)
				}
			}),
			intAttr(0x0003, MinHeatSetpoint), // AbsMinHeatSetpointLimit
			intAttr(0x0004, MaxHeatSetpoint), // AbsMaxHeatSetpointLimit
			intAttr(0x0005, MinHeatSetpoint), // AbsMinCoolSetpointLimit
			intAttr(0x0006, MaxHeatSetpoint), // AbsMaxCoolSetpointLimit
			a.setpointAttr(attrOccupiedCoolingSetpoint, SystemModeHeat),
			a.setpointAttr(attrOccupiedHeatingSetpoint, SystemModeCool),
			intAttr(0x0015, MinHeatSetpoint), // MinHeatSetpointLimit
			intAttr(0x0016, MaxHeatSetpoint), // MaxHeatSetpointLimit
			intAttr(0x0017, MinHeatSetpoint), // MinCoolSetpointLimit
			intAttr(0x0018, MaxHeatSetpoint), // MaxCoolSetpointLimit
			fixed(attrMinSetpointDeadBand, 0, true),
			fixed(attrControlSequenceOfOperation, controlCoolingAndHeating, false),
			{
				ID: attrSystemMode,
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error {
					w.PutUint(tag, a.systemMode())
					return nil
				},
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Uint()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					switch v {
					case SystemModeOff:
						a.command(false, a.mode)
					case SystemModeAuto, SystemModeCool, SystemModeHeat, SystemModeFanOnly, SystemModeDry:
						a.command(true, v)
					default:
						return im.StatusConstraintError
					}
					return im.StatusSuccess
				},
				WritePrivilege: im.PrivilegeManage,
			},
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: a.raiseLower}, // SetpointRaiseLower
		},
	}
	return a
}

// fixed is a writable attribute that only ever has v, a signed integer or an unsigned one or enum:
// writing v succeeds, and anything else can't be, as for a setting the device doesn't have.
func fixed(id uint32, v int64, signed bool) im.Attribute {
	return im.Attribute{
		ID: id,
		Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error {
			if signed {
				w.PutInt(tag, v)
			} else {
				w.PutUint(tag, uint64(v))
			}
			return nil
		},
		Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
			var got int64
			var err error
			if signed {
				got, err = r.Int()
			} else {
				var u uint64
				u, err = r.Uint()
				got = int64(min(u, math.MaxInt64))
			}
			if err != nil || op != im.Replace {
				return im.StatusInvalidDataType
			}
			if got != v {
				return im.StatusConstraintError
			}
			return im.StatusSuccess
		},
		WritePrivilege: im.PrivilegeManage,
	}
}

// setpointAttr is one of the occupied setpoints, which are both the one setpoint. In unused, the
// other side's mode, a write is accepted and not sent, so a controller's slider for the side not in
// use can't move the target.
func (a *AirConditioner) setpointAttr(id uint32, unused uint64) im.Attribute {
	return im.Attribute{
		ID:   id,
		Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutInt(tag, a.setpoint); return nil },
		Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
			v, err := r.Int()
			if err != nil || op != im.Replace {
				return im.StatusInvalidDataType
			}
			if v < MinHeatSetpoint || v > MaxHeatSetpoint {
				return im.StatusConstraintError
			}
			if a.mode != unused {
				a.setTo(v)
			}
			return im.StatusSuccess
		},
	}
}

// Cluster returns the cluster to add to an endpoint.
func (a *AirConditioner) Cluster() *im.Cluster { return a.cluster }

// SetLocal records the temperature the unit measures, in degrees Celsius, within what
// LocalTemperature can hold. Call it inside Node.Update.
func (a *AirConditioner) SetLocal(celsius float64) {
	v := min(max(hundredths(celsius), minLocal), math.MaxInt16)
	if a.local == nil || *a.local != v {
		a.local = &v
		a.cluster.Changed(attrLocalTemperature)
	}
}

// SetSetpoint records the unit's setpoint, in degrees Celsius, as the device reports it: both
// occupied setpoints. One outside the limits shows as the nearest limit. Call it inside
// Node.Update.
func (a *AirConditioner) SetSetpoint(celsius float64) {
	a.set(clampSetpoint(hundredths(celsius)))
}

// SetOn records whether the unit is on: SystemMode Off, or its mode. Call it inside Node.Update.
func (a *AirConditioner) SetOn(on bool) {
	if on != a.on {
		a.on = on
		a.cluster.Changed(attrSystemMode)
	}
}

// SetMode records the unit's mode, a SystemMode other than Off, which SystemMode shows while it's
// on. Call it inside Node.Update.
func (a *AirConditioner) SetMode(mode uint64) {
	if mode != a.mode {
		a.mode = mode
		if a.on {
			a.cluster.Changed(attrSystemMode)
		}
	}
}

func (a *AirConditioner) systemMode() uint64 {
	if a.on {
		return a.mode
	}
	return SystemModeOff
}

func (a *AirConditioner) set(v int64) {
	if v != a.setpoint {
		a.setpoint = v
		a.cluster.Changed(attrOccupiedHeatingSetpoint)
		a.cluster.Changed(attrOccupiedCoolingSetpoint)
	}
}

func (a *AirConditioner) setTo(v int64) {
	a.set(v)
	if a.OnSetpoint != nil {
		a.OnSetpoint(float64(v) / 100)
	}
}

func (a *AirConditioner) command(on bool, mode uint64) {
	a.SetMode(mode)
	a.SetOn(on)
	if a.OnSystemMode != nil {
		a.OnSystemMode(on, mode)
	}
}

// raiseLower is SetpointRaiseLower: Mode Heat (0), Cool (1) or Both (2) moves the setpoint by
// Amount tenths of a degree, within the limits, unless it names only the side not in use.
func (a *AirConditioner) raiseLower(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	mode, amount, err := readRaiseLower(fields)
	if err != nil {
		return nil, err
	}
	switch {
	case mode > 2:
		return nil, im.StatusConstraintError
	case mode == 0 && a.mode == SystemModeCool, mode == 1 && a.mode == SystemModeHeat:
		return nil, nil
	}
	a.setTo(clampSetpoint(a.setpoint + amount*10))
	return nil, nil
}
