package clusters

import (
	"math"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// IDWindowCovering is the Window Covering cluster.
const (
	IDWindowCovering uint32 = 0x0102
)

// DeviceWindowCovering is the Window Covering device type, at Matter 1.3's revision. It needs
// Identify, which every bridged endpoint has, and Window Covering. Groups and Scenes Management are
// only for a device that starts things itself.
var DeviceWindowCovering = im.DeviceType{ID: 0x0202, Revision: 3}

// The Window Covering commands.
const (
	CommandUpOrOpen           = 0x00
	CommandDownOrClose        = 0x01
	CommandStopMotion         = 0x02
	CommandGoToLiftPercentage = 0x05
)

// The Window Covering attributes.
const (
	attrCurrentLiftPercentage = 0x0008
	attrOperationalStatus     = 0x000A
	attrTargetLift100ths      = 0x000B
	attrCurrentLift100ths     = 0x000E
	attrMode                  = 0x0017
)

// The OperationalStatus values of each of its fields (MovementStatus).
const (
	MovementStopped = 0
	MovementOpening = 1
	MovementClosing = 2
)

// closed is fully closed, in Matter's hundredths of a percent: 0 is fully open.
const closed = 10000

// nearLift is how far apart, in hundredths of a percent, a report can be from a target and still
// have reached it: a driver rounds to whole percents.
const nearLift = 50

// Covering is a Window Covering cluster with the Lift and PositionAwareLift features: a blind,
// shade or shutter that goes up and down to a position. Positions are in hundredths of a percent
// closed, 0 open and 10000 closed.
//
// Target comes from commands and Current only from the device's reports. A move is in progress, and
// OperationalStatus says so, until a report reaches its target or it has had time to settle, when
// Target follows Current.
type Covering struct {
	cluster *im.Cluster
	current *int64
	target  *int64
	moving  bool
	// reported is set by a report since the last move of a position other than where it started.
	reported bool
	// from is where the last move started.
	from *int64
	// stopSettle stops the last move's settle, if it's waiting.
	stopSettle func()
	// move counts moves, so that a settle meant for an earlier one does nothing.
	move int
	// OnMove is called, under the Node's lock, when a client moves it, with the target in percent
	// closed. It must not block.
	OnMove func(percent float64)
	// OnCommand, if set, is called, under the Node's lock, with each command a client sends: its
	// name, and the target it moves to in percent closed, or nil for one that moves nothing, as a
	// StopMotion with nothing moving. It's called before OnMove, and must not block.
	OnCommand func(command string, target *float64)
	// AfterSettle runs f, under the Node's lock, once a move has had as long as it takes, unless the
	// function it returns is called first. Nil leaves a move until a report reaches its target.
	AfterSettle func(f func()) (stop func())
}

// NewCovering returns a Window Covering cluster for a roller shade.
func NewCovering() *Covering {
	c := &Covering{}
	const featureLift, featurePositionAwareLift = 1 << 0, 1 << 2
	const configOperational, configLiftPositionAware = 1 << 0, 1 << 3
	nullable := func(v func() *int64, scale int64) func(w *tlv.Writer, tag tlv.Tag) {
		return func(w *tlv.Writer, tag tlv.Tag) {
			if p := v(); p == nil {
				w.PutNull(tag)
			} else {
				w.PutUint(tag, uint64((*p+scale/2)/scale))
			}
		}
	}
	go100ths := func(command string, target int64) func(*im.Request, *tlv.Reader) (*im.Response, error) {
		return func(*im.Request, *tlv.Reader) (*im.Response, error) { c.goTo(command, target); return nil, nil }
	}
	c.cluster = &im.Cluster{
		ID: IDWindowCovering, Revision: 5, FeatureMap: featureLift | featurePositionAwareLift,
		Attributes: []im.Attribute{
			uintAttr(0x0000, 0), // Type: RollerShade
			uintAttr(0x0007, configOperational|configLiftPositionAware),                            // ConfigStatus
			readOnly(attrCurrentLiftPercentage, nullable(func() *int64 { return c.current }, 100)), // CurrentPositionLiftPercentage
			readOnly(attrOperationalStatus, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, c.operationalStatus()) }),
			readOnly(attrTargetLift100ths, nullable(func() *int64 { return c.target }, 1)), // TargetPositionLiftPercent100ths
			uintAttr(0x000D, 0), // EndProductType: RollerShade
			readOnly(attrCurrentLift100ths, nullable(func() *int64 { return c.current }, 1)), // CurrentPositionLiftPercent100ths
			{
				ID:   attrMode,
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutUint(tag, 0); return nil },
				// A bridge can't reverse the motor, calibrate it or put it in maintenance, so only the
				// normal mode can be written.
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Uint()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					if v != 0 {
						return im.StatusConstraintError
					}
					return im.StatusSuccess
				},
				WritePrivilege: im.PrivilegeManage,
			},
		},
		Commands: []im.Command{
			{ID: CommandUpOrOpen, Invoke: go100ths("UpOrOpen", 0)},
			{ID: CommandDownOrClose, Invoke: go100ths("DownOrClose", closed)},
			{ID: CommandStopMotion, Invoke: c.stop},
			{ID: CommandGoToLiftPercentage, Invoke: c.goToPercentage},
		},
	}
	return c
}

// Cluster returns the cluster to add to an endpoint.
func (c *Covering) Cluster() *im.Cluster { return c.cluster }

// SetCurrent records where the device says it is, in percent closed. A report at a move's target
// ends the move. Call it inside Node.Update.
func (c *Covering) SetCurrent(percent float64) {
	v := min(max(int64(math.Round(percent*100)), 0), closed)
	if c.current == nil || *c.current != v {
		c.current = &v
		c.cluster.Changed(attrCurrentLift100ths)
		c.cluster.Changed(attrCurrentLiftPercentage)
	}
	if c.from == nil || *c.from != v {
		c.reported = true
	}
	if c.moving && c.target != nil && abs(*c.target-v) <= nearLift {
		c.setMoving(false)
	}
	if !c.moving {
		c.setTarget(v)
	}
}

// Cancel ends a move that won't happen, as when a write is refused: Target goes back to Current.
// Call it inside Node.Update.
func (c *Covering) Cancel() {
	c.move++
	c.waitNoLonger()
	c.settle()
}

func (c *Covering) waitNoLonger() {
	if c.stopSettle != nil {
		c.stopSettle()
		c.stopSettle = nil
	}
}

// command tells OnCommand of a command, with its target in hundredths, or nil.
func (c *Covering) command(name string, target *int64) {
	if c.OnCommand == nil {
		return
	}
	var percent *float64
	if target != nil {
		p := float64(*target) / 100
		percent = &p
	}
	c.OnCommand(name, percent)
}

func (c *Covering) goTo(command string, target int64) {
	c.command(command, &target)
	c.setTarget(target)
	c.setMoving(c.current == nil || abs(*c.current-target) > nearLift)
	c.reported, c.from = false, c.current
	c.move++
	c.waitNoLonger()
	if c.OnMove != nil {
		c.OnMove(float64(target) / 100)
	}
	if c.AfterSettle != nil && c.moving {
		move := c.move
		c.stopSettle = c.AfterSettle(func() {
			if c.move == move {
				c.stopSettle = nil
				c.settle()
			}
		})
	}
}

// settle ends a move: Target follows Current, and is unknown with it.
func (c *Covering) settle() {
	c.setMoving(false)
	switch {
	case c.current != nil:
		c.setTarget(*c.current)
	case c.target != nil:
		c.target = nil
		c.cluster.Changed(attrTargetLift100ths)
	}
}

func (c *Covering) setTarget(v int64) {
	if c.target == nil || *c.target != v {
		c.target = &v
		c.cluster.Changed(attrTargetLift100ths)
	}
}

func (c *Covering) setMoving(moving bool) {
	if moving != c.moving {
		c.moving = moving
		c.cluster.Changed(attrOperationalStatus)
	}
}

// operationalStatus is OperationalStatus: the Global and Lift fields both say which way it moves.
func (c *Covering) operationalStatus() uint64 {
	if !c.moving || c.target == nil {
		return MovementStopped
	}
	from := int64(0)
	if c.current != nil {
		from = *c.current
	}
	status := uint64(MovementOpening)
	if *c.target > from {
		status = MovementClosing
	}
	return status | status<<2
}

// stop is StopMotion. With no capability to stop a device, it writes the last reported position,
// which stops a move on the way. With no report since the move, the device would go back to where
// it started, which is worse than not stopping, so it fails.
func (c *Covering) stop(*im.Request, *tlv.Reader) (*im.Response, error) {
	switch {
	case !c.moving:
		c.command("StopMotion", nil)
		return nil, nil
	case !c.reported || c.current == nil:
		c.command("StopMotion", nil)
		return nil, im.StatusFailure
	}
	c.goTo("StopMotion", *c.current)
	return nil, nil
}

// goToPercentage is GoToLiftPercentage. Its target is in hundredths: field 0 since Matter 1.4 and in
// the SDK controllers are built with, and field 1 in 1.3's data model, whose field 0 was a whole
// percent that controllers don't send. So field 1 wins, and field 0 is hundredths.
func (c *Covering) goToPercentage(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	if fields == nil {
		return nil, im.StatusInvalidCommand
	}
	var field0, field1 uint64
	seen0, seen1 := false, false
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			field0, err = r.Uint()
			seen0 = true
		case 1:
			field1, err = r.Uint()
			seen1 = true
		}
		return err
	}); err != nil {
		return nil, im.StatusInvalidCommand
	}
	hundredths := field0
	switch {
	case seen1:
		hundredths = field1
	case !seen0:
		return nil, im.StatusInvalidCommand
	}
	if hundredths > closed {
		return nil, im.StatusConstraintError
	}
	c.goTo("GoToLiftPercentage", int64(hundredths))
	return nil, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
