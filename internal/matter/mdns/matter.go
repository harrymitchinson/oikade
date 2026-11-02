package mdns

import (
	"fmt"
	"strconv"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/setup"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Matter's service types (Core spec, 4.3).
const (
	CommissionableType = "_matterc._udp" // a node that can be commissioned (4.3.1)
	OperationalType    = "_matter._tcp"  // a commissioned node, once per fabric (4.3.2)
)

// CommissioningMode is the CM key: whether the commissioning window is open, and how.
type CommissioningMode uint8

// Commissioning modes.
const (
	CommissioningClosed   CommissioningMode = 0
	CommissioningBasic    CommissioningMode = 1 // opened with the passcode the node was made with
	CommissioningEnhanced CommissioningMode = 2 // opened by an administrator, with a new passcode
)

// maxMRPInterval is the most that SII and SAI may say (Core spec, 4.3.4): an hour.
const maxMRPInterval = time.Hour

// NewInstanceName returns a random commissionable instance name: 64 bits in 16 upper-case hex
// digits (Core spec, 4.3.1.1). A node picks a new one each time it advertises afresh, so that it
// can't be followed by its name.
func NewInstanceName() string { return randomHex(8) }

// Commissionable is a node's commissionable service (Core spec, 4.3.1).
type Commissionable struct {
	// Instance is the instance name, from NewInstanceName.
	Instance string
	// Discriminator is the 12-bit long discriminator, as in the onboarding payload.
	Discriminator uint16
	VendorID      uint16 // 0 leaves out VP and the _V subtype
	ProductID     uint16 // 0 leaves it out of VP
	Mode          CommissioningMode
	DeviceType    uint32 // the primary device type; 0 leaves out DT and the _T subtype
	DeviceName    string // at most 32 bytes; empty leaves out DN
	PairingHint   uint16 // a bitmap of how to put the node in commissioning mode; 0 leaves out PH
	// MRP is the node's retransmission parameters. Zero fields leave out their keys, and the
	// commissioner assumes the spec's defaults.
	MRP transport.MRPConfig
	// TCP is the T key, a bitmap of TCP support; 0 leaves it out.
	TCP uint8
	// Port is the Matter port; 0 means 5540.
	Port uint16
}

// Service returns the DNS-SD service that advertises c.
func (c Commissionable) Service() (Service, error) {
	switch {
	case c.Discriminator > setup.MaxDiscriminator:
		return Service{}, fmt.Errorf("mdns: discriminator %d is more than 12 bits", c.Discriminator)
	case len(c.DeviceName) > 32:
		return Service{}, fmt.Errorf("mdns: device name %q is longer than 32 bytes", c.DeviceName)
	}
	svc := Service{
		Instance: c.Instance,
		Type:     CommissionableType,
		Port:     orPort(c.Port),
		Subtypes: []string{
			"_L" + strconv.Itoa(int(c.Discriminator)),
			"_S" + strconv.Itoa(int(c.Discriminator>>8)), // the short discriminator: the top 4 bits
		},
		Text: []string{
			"D=" + strconv.Itoa(int(c.Discriminator)),
			"CM=" + strconv.Itoa(int(c.Mode)),
		},
	}
	if c.VendorID != 0 {
		svc.Subtypes = append(svc.Subtypes, "_V"+strconv.Itoa(int(c.VendorID)))
		vp := "VP=" + strconv.Itoa(int(c.VendorID))
		if c.ProductID != 0 {
			vp += "+" + strconv.Itoa(int(c.ProductID))
		}
		svc.Text = append(svc.Text, vp)
	}
	if c.DeviceType != 0 {
		svc.Subtypes = append(svc.Subtypes, "_T"+strconv.FormatUint(uint64(c.DeviceType), 10))
		svc.Text = append(svc.Text, "DT="+strconv.FormatUint(uint64(c.DeviceType), 10))
	}
	if c.Mode != CommissioningClosed {
		svc.Subtypes = append(svc.Subtypes, "_CM")
	}
	if c.DeviceName != "" {
		svc.Text = append(svc.Text, "DN="+c.DeviceName)
	}
	svc.Text = append(svc.Text, commonText(c.MRP, c.TCP)...)
	if c.PairingHint != 0 {
		svc.Text = append(svc.Text, "PH="+strconv.Itoa(int(c.PairingHint)))
	}
	return svc, nil
}

// Operational is a commissioned node's service on one fabric (Core spec, 4.3.2).
type Operational struct {
	CompressedFabricID uint64
	NodeID             uint64
	MRP                transport.MRPConfig // as in Commissionable
	TCP                uint8               // as in Commissionable
	Port               uint16              // 0 means 5540
}

// Service returns the DNS-SD service that advertises o: the instance
// "<compressed fabric ID>-<node ID>", each in 16 upper-case hex digits, with the subtype
// "_I<compressed fabric ID>".
func (o Operational) Service() Service {
	fabric := fmt.Sprintf("%016X", o.CompressedFabricID)
	return Service{
		Instance: fmt.Sprintf("%s-%016X", fabric, o.NodeID),
		Type:     OperationalType,
		Subtypes: []string{"_I" + fabric},
		Port:     orPort(o.Port),
		Text:     commonText(o.MRP, o.TCP),
	}
}

// commonText returns the TXT keys that both services carry (Core spec, 4.3.4): SII, SAI and SAT in
// milliseconds, and T.
func commonText(mrp transport.MRPConfig, tcp uint8) []string {
	var txt []string
	if mrp.Idle > 0 {
		txt = append(txt, "SII="+strconv.FormatInt(min(mrp.Idle, maxMRPInterval).Milliseconds(), 10))
	}
	if mrp.Active > 0 {
		txt = append(txt, "SAI="+strconv.FormatInt(min(mrp.Active, maxMRPInterval).Milliseconds(), 10))
	}
	if mrp.ActiveThreshold > 0 {
		txt = append(txt, "SAT="+strconv.FormatInt(min(mrp.ActiveThreshold.Milliseconds(), 65535), 10))
	}
	if tcp != 0 {
		txt = append(txt, "T="+strconv.Itoa(int(tcp)))
	}
	return txt
}

func orPort(p uint16) uint16 {
	if p == 0 {
		return transport.Port
	}
	return p
}
