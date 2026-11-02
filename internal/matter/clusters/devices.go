package clusters

import (
	"math"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// The application clusters a bridged device can have (Matter Application Cluster Specification).
const (
	IDLevelControl                uint32 = 0x0008
	IDPowerSource                 uint32 = 0x002F
	IDBooleanState                uint32 = 0x0045
	IDTemperatureMeasurement      uint32 = 0x0402
	IDRelativeHumidityMeasurement uint32 = 0x0405
	IDOccupancySensing            uint32 = 0x0406
)

// The device types the mapping uses (Matter Device Library Specification).
var (
	DeviceDimmableLight       = im.DeviceType{ID: 0x0101, Revision: 3}
	DeviceDimmablePlug        = im.DeviceType{ID: 0x010B, Revision: 4}
	DeviceMountedOnOffControl = im.DeviceType{ID: 0x010F, Revision: 1}
	DeviceMountedDimmableLoad = im.DeviceType{ID: 0x0110, Revision: 1}
	DeviceContactSensor       = im.DeviceType{ID: 0x0015, Revision: 2}
	DeviceOccupancySensor     = im.DeviceType{ID: 0x0107, Revision: 4}
	DeviceTemperatureSensor   = im.DeviceType{ID: 0x0302, Revision: 2}
	DeviceHumiditySensor      = im.DeviceType{ID: 0x0307, Revision: 2}
	DevicePowerSource         = im.DeviceType{ID: 0x0011, Revision: 1}
)

// NewLightOnOff returns an On/Off cluster with the Lighting feature, which the light device
// types need. The timed and effect commands act at once: a bridge can't fade the real light.
func NewLightOnOff() *OnOff {
	o := NewOnOff()
	const featureLighting = 1 << 0
	o.cluster.FeatureMap = featureLighting
	o.cluster.Attributes = append(o.cluster.Attributes,
		readOnly(0x4000, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) }), // GlobalSceneControl
		uintAttr(0x4001, 0), // OnTime
		uintAttr(0x4002, 0), // OffWaitTime
		readOnly(0x4003, func(w *tlv.Writer, tag tlv.Tag) { w.PutNull(tag) }), // StartUpOnOff: the device's own
	)
	act := func(on bool) func(*im.Request, *tlv.Reader) (*im.Response, error) {
		return func(*im.Request, *tlv.Reader) (*im.Response, error) { o.command(on); return nil, nil }
	}
	o.cluster.Commands = append(o.cluster.Commands,
		im.Command{ID: 0x40, Invoke: act(false)}, // OffWithEffect
		im.Command{ID: 0x41, Invoke: act(true)},  // OnWithRecallGlobalScene
		im.Command{ID: 0x42, Invoke: act(true)},  // OnWithTimedOff
	)
	return o
}

// Level is a Level Control cluster for a dimmable device, as a percentage. Matter's levels run
// from 1 to 254; 0 to 100 percent maps onto them.
type Level struct {
	cluster *im.Cluster
	level   uint8
	options uint64
	// OnChange is called, under the Node's lock, when a client sets the level. withOnOff is set
	// for the commands that also turn the device on, or off at the minimum level, which atMinimum
	// says. It must not block.
	OnChange func(percent float64, withOnOff, atMinimum bool)
}

const (
	minLevel = 1
	maxLevel = 254
)

// NewLevel returns a Level Control cluster with the On/Off and Lighting features.
func NewLevel() *Level {
	l := &Level{level: maxLevel}
	const featureOnOff, featureLighting = 1 << 0, 1 << 1
	move := func(withOnOff bool) func(*im.Request, *tlv.Reader) (*im.Response, error) {
		return func(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
			var level uint64
			seen := false
			if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
				if tag == 0 {
					level, err = r.Uint()
					seen = true
				}
				return err
			}); err != nil || !seen {
				return nil, im.StatusInvalidCommand
			}
			if level > maxLevel {
				return nil, im.StatusConstraintError
			}
			l.command(uint8(max(level, minLevel)), withOnOff)
			return nil, nil
		}
	}
	// Move and Step have a rate or a step but no target: they go to the end they move towards,
	// since the bridge can't run the transition on the real device.
	toward := func(modeTag uint8, withOnOff bool, step bool) func(*im.Request, *tlv.Reader) (*im.Response, error) {
		return func(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
			var mode, size uint64
			seen := false
			if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
				switch tag {
				case modeTag:
					mode, err = r.Uint()
					seen = true
				case 1:
					if step {
						size, err = r.Uint()
					}
				}
				return err
			}); err != nil || !seen || mode > 1 {
				return nil, im.StatusInvalidCommand
			}
			target := uint8(maxLevel)
			if step {
				v := int(l.level) + int(size)
				if mode == 1 {
					v = int(l.level) - int(size)
				}
				target = uint8(min(max(v, minLevel), maxLevel))
			} else if mode == 1 {
				target = minLevel
			}
			l.command(target, withOnOff)
			return nil, nil
		}
	}
	stop := func(*im.Request, *tlv.Reader) (*im.Response, error) { return nil, nil }
	l.cluster = &im.Cluster{
		ID: IDLevelControl, Revision: 6, FeatureMap: featureOnOff | featureLighting,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, uint64(l.level)) }), // CurrentLevel
			uintAttr(0x0001, 0),        // RemainingTime
			uintAttr(0x0002, minLevel), // MinLevel
			uintAttr(0x0003, maxLevel), // MaxLevel
			{
				ID:   0x000F, // Options
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutUint(tag, l.options); return nil },
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Uint()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					if v > 3 {
						return im.StatusConstraintError
					}
					if v != l.options {
						l.options = v
						l.cluster.Changed(0x000F)
					}
					return im.StatusSuccess
				},
			},
			readOnly(0x0011, func(w *tlv.Writer, tag tlv.Tag) { w.PutNull(tag) }), // OnLevel
			readOnly(0x4000, func(w *tlv.Writer, tag tlv.Tag) { w.PutNull(tag) }), // StartUpCurrentLevel
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: move(false)},             // MoveToLevel
			{ID: 0x01, Invoke: toward(0, false, false)}, // Move
			{ID: 0x02, Invoke: toward(0, false, true)},  // Step
			{ID: 0x03, Invoke: stop},                    // Stop
			{ID: 0x04, Invoke: move(true)},              // MoveToLevelWithOnOff
			{ID: 0x05, Invoke: toward(0, true, false)},  // MoveWithOnOff
			{ID: 0x06, Invoke: toward(0, true, true)},   // StepWithOnOff
			{ID: 0x07, Invoke: stop},                    // StopWithOnOff
		},
	}
	return l
}

// Cluster returns the cluster to add to an endpoint.
func (l *Level) Cluster() *im.Cluster { return l.cluster }

// Set records the device's level, in percent. Call it inside Node.Update.
func (l *Level) Set(percent float64) {
	v := percentToLevel(percent)
	if v != l.level {
		l.level = v
		l.cluster.Changed(0x0000)
	}
}

func (l *Level) command(level uint8, withOnOff bool) {
	if level != l.level {
		l.level = level
		l.cluster.Changed(0x0000)
	}
	if l.OnChange != nil {
		l.OnChange(levelToPercent(level), withOnOff, level == minLevel)
	}
}

func percentToLevel(p float64) uint8 {
	p = min(max(p, 0), 100)
	return uint8(math.Round(minLevel + p/100*(maxLevel-minLevel)))
}

// levelToPercent never rounds a level above the minimum down to 0%, which would read as off.
func levelToPercent(l uint8) float64 {
	p := math.Round(float64(l-minLevel) / (maxLevel - minLevel) * 100)
	if p == 0 && l > minLevel {
		p = 1
	}
	return p
}

// Occupancy is an Occupancy Sensing cluster for a passive infrared sensor.
type Occupancy struct {
	cluster  *im.Cluster
	occupied bool
}

// NewOccupancy returns an Occupancy Sensing cluster.
func NewOccupancy() *Occupancy {
	o := &Occupancy{}
	const featurePIR = 1 << 1
	o.cluster = &im.Cluster{
		ID: IDOccupancySensing, Revision: 5, FeatureMap: featurePIR,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { // Occupancy
				w.PutUint(tag, uint64(boolBit(o.occupied)))
			}),
			uintAttr(0x0001, 0), // OccupancySensorType: PIR
			uintAttr(0x0002, 1), // OccupancySensorTypeBitmap: PIR
		},
	}
	return o
}

// Cluster returns the cluster to add to an endpoint.
func (o *Occupancy) Cluster() *im.Cluster { return o.cluster }

// Set records whether the sensor detects occupancy. Call it inside Node.Update.
func (o *Occupancy) Set(occupied bool) {
	if occupied != o.occupied {
		o.occupied = occupied
		o.cluster.Changed(0x0000)
	}
}

// BooleanState is a Boolean State cluster. For a contact sensor, true means contact: closed.
type BooleanState struct {
	cluster *im.Cluster
	state   bool
}

// NewBooleanState returns a Boolean State cluster.
func NewBooleanState() *BooleanState {
	b := &BooleanState{}
	b.cluster = &im.Cluster{
		ID: IDBooleanState, Revision: 1,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, b.state) }), // StateValue
		},
	}
	return b
}

// Cluster returns the cluster to add to an endpoint.
func (b *BooleanState) Cluster() *im.Cluster { return b.cluster }

// Set records the state. Call it inside Node.Update.
func (b *BooleanState) Set(state bool) {
	if state != b.state {
		b.state = state
		b.cluster.Changed(0x0000)
	}
}

// Measurement is a measurement cluster with a nullable MeasuredValue in hundredths of its unit:
// Temperature Measurement (°C) or Relative Humidity Measurement (%).
type Measurement struct {
	cluster *im.Cluster
	value   *int64
	signed  bool
}

// NewTemperature returns a Temperature Measurement cluster, from -100 to 100 °C.
func NewTemperature() *Measurement {
	return newMeasurement(IDTemperatureMeasurement, 4, true, -10000, 10000)
}

// NewHumidity returns a Relative Humidity Measurement cluster, from 0 to 100 %.
func NewHumidity() *Measurement {
	return newMeasurement(IDRelativeHumidityMeasurement, 3, false, 0, 10000)
}

func newMeasurement(id uint32, revision uint16, signed bool, lo, hi int64) *Measurement {
	m := &Measurement{signed: signed}
	put := func(v int64) func(w *tlv.Writer, tag tlv.Tag) {
		return func(w *tlv.Writer, tag tlv.Tag) {
			if signed {
				w.PutInt(tag, v)
			} else {
				w.PutUint(tag, uint64(v))
			}
		}
	}
	m.cluster = &im.Cluster{
		ID: id, Revision: revision,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { // MeasuredValue
				if m.value == nil {
					w.PutNull(tag)
				} else {
					put(*m.value)(w, tag)
				}
			}),
			readOnly(0x0001, put(lo)), // MinMeasuredValue
			readOnly(0x0002, put(hi)), // MaxMeasuredValue
		},
	}
	return m
}

// Cluster returns the cluster to add to an endpoint.
func (m *Measurement) Cluster() *im.Cluster { return m.cluster }

// Set records a measurement in the cluster's unit. Call it inside Node.Update.
func (m *Measurement) Set(v float64) {
	h := int64(math.Round(v * 100))
	if m.value == nil || *m.value != h {
		m.value = &h
		m.cluster.Changed(0x0000)
	}
}

// Battery is a Power Source cluster for a replaceable battery, with the charge in percent.
type Battery struct {
	cluster *im.Cluster
	percent *uint8
	// endpoint is the endpoint the source powers, for EndpointList.
	endpoint uint16
}

// LowBattery is the charge, in percent, below which the battery reports a warning.
const LowBattery = 20

// NewBattery returns a Power Source cluster for the battery of the device on endpoint.
func NewBattery(endpoint uint16) *Battery {
	b := &Battery{endpoint: endpoint}
	const featureBattery, featureReplaceable = 1 << 1, 1 << 3
	b.cluster = &im.Cluster{
		ID: IDPowerSource, Revision: 3, FeatureMap: featureBattery | featureReplaceable,
		Attributes: []im.Attribute{
			uintAttr(0x0000, 1), // Status: Active
			uintAttr(0x0001, 0), // Order
			readOnly(0x0002, func(w *tlv.Writer, tag tlv.Tag) { w.PutUTF8(tag, "Battery") }), // Description
			readOnly(0x000C, func(w *tlv.Writer, tag tlv.Tag) { // BatPercentRemaining, in half percent
				if b.percent == nil {
					w.PutNull(tag)
				} else {
					w.PutUint(tag, uint64(*b.percent)*2)
				}
			}),
			readOnly(0x000E, func(w *tlv.Writer, tag tlv.Tag) { // BatChargeLevel: OK or Warning
				w.PutUint(tag, uint64(boolBit(b.percent != nil && *b.percent < LowBattery)))
			}),
			readOnly(0x000F, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, false) }), // BatReplacementNeeded
			uintAttr(0x0010, 1), // BatReplaceability: UserReplaceable
			readOnly(0x001F, func(w *tlv.Writer, tag tlv.Tag) { // EndpointList
				w.StartArray(tag)
				w.PutUint(tlv.Anonymous, uint64(b.endpoint))
				w.End()
			}),
		},
	}
	return b
}

// Cluster returns the cluster to add to an endpoint.
func (b *Battery) Cluster() *im.Cluster { return b.cluster }

// Set records the charge in percent. Call it inside Node.Update.
func (b *Battery) Set(percent float64) {
	p := uint8(math.Round(min(max(percent, 0), 100)))
	if b.percent == nil || *b.percent != p {
		low := b.percent != nil && *b.percent < LowBattery
		b.percent = &p
		b.cluster.Changed(0x000C)
		if low != (p < LowBattery) {
			b.cluster.Changed(0x000E)
		}
	}
}

func boolBit(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
