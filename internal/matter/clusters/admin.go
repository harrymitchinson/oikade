package clusters

import (
	"time"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// CommissioningWindowStatusEnum.
const (
	windowClosed   = 0
	windowEnhanced = 1
	windowBasic    = 2
)

// Administrator Commissioning's cluster status codes.
const (
	adminBusy               = 2
	adminPAKEParameterError = 3
	adminWindowNotOpen      = 4
)

// The commissioning window's limits (Core spec, 5.4.2.3.1).
const (
	minWindowTimeout = 180 * time.Second
	maxWindowTimeout = 900 * time.Second
)

// Window is a commissioning window that an administrator opened.
type Window struct {
	// Basic windows take the node's own passcode. Enhanced ones take the verifier below.
	Basic         bool
	Verifier      mcrypto.Verifier
	Salt          []byte
	Iterations    uint32
	Discriminator uint16
	Timeout       time.Duration
}

// window is Administrator Commissioning's state. The Node's lock guards it.
type window struct {
	status      uint8
	adminFabric uint8
	adminVendor uint16
	timer       *time.Timer
	generation  int
}

func (c *Commissioning) administratorCommissioning() *im.Cluster {
	const featureBasic = 1 << 0
	admin := im.PrivilegeAdminister
	nullable := func(id uint32, v func() (uint64, bool)) im.Attribute {
		return readOnly(id, func(w *tlv.Writer, tag tlv.Tag) {
			if x, ok := v(); ok {
				w.PutUint(tag, x)
			} else {
				w.PutNull(tag)
			}
		})
	}
	return &im.Cluster{
		ID: IDAdministratorCommissioning, Revision: 1, FeatureMap: featureBasic,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, uint64(c.window.status)) }),
			nullable(0x0001, func() (uint64, bool) { return uint64(c.window.adminFabric), c.window.adminFabric != 0 }),
			nullable(0x0002, func() (uint64, bool) { return uint64(c.window.adminVendor), c.window.adminFabric != 0 }),
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: c.openCommissioningWindow, Timed: true, Privilege: admin},
			{ID: 0x01, Invoke: c.openBasicCommissioningWindow, Timed: true, Privilege: admin},
			{ID: 0x02, Invoke: c.revokeCommissioning, Timed: true, Privilege: admin},
		},
	}
}

func busy() error { return im.ClusterStatus{Status: im.StatusFailure, Code: adminBusy} }

func (c *Commissioning) openCommissioningWindow(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var timeout, discriminator, iterations uint64
	var verifier, salt []byte
	var seen [5]bool
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			timeout, err = r.Uint()
		case 1:
			verifier, err = r.Bytes()
		case 2:
			discriminator, err = r.Uint()
		case 3:
			iterations, err = r.Uint()
		case 4:
			salt, err = r.Bytes()
		default:
			return nil
		}
		seen[tag] = true
		return err
	}); err != nil || seen != [5]bool{true, true, true, true, true} {
		return nil, im.StatusInvalidCommand
	}
	if c.window.status != windowClosed || c.failSafe.armed {
		return nil, busy()
	}
	d := time.Duration(timeout) * time.Second
	if d < minWindowTimeout || d > maxWindowTimeout || discriminator > 0xFFF {
		return nil, im.StatusInvalidCommand
	}
	v, err := mcrypto.ParseVerifier(verifier)
	if err != nil || iterations < 1000 || iterations > 100000 || len(salt) < 16 || len(salt) > 32 {
		return nil, im.ClusterStatus{Status: im.StatusFailure, Code: adminPAKEParameterError}
	}
	c.openWindow(req, Window{
		Verifier: v, Salt: append([]byte(nil), salt...), Iterations: uint32(iterations),
		Discriminator: uint16(discriminator), Timeout: d,
	})
	return nil, nil
}

func (c *Commissioning) openBasicCommissioningWindow(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var timeout uint64
	seen := false
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		if tag == 0 {
			timeout, err = r.Uint()
			seen = true
		}
		return err
	}); err != nil || !seen {
		return nil, im.StatusInvalidCommand
	}
	if c.window.status != windowClosed || c.failSafe.armed {
		return nil, busy()
	}
	d := time.Duration(timeout) * time.Second
	if d < minWindowTimeout || d > maxWindowTimeout {
		return nil, im.StatusInvalidCommand
	}
	c.openWindow(req, Window{Basic: true, Timeout: d})
	return nil, nil
}

// openWindow opens w, and closes it when it times out. Call it with the Node's lock.
func (c *Commissioning) openWindow(req *im.Request, w Window) {
	status := uint8(windowEnhanced)
	if w.Basic {
		status = windowBasic
	}
	var vendor uint16
	if f, ok := c.fabrics.Fabric(req.FabricIndex); ok {
		vendor = f.VendorID
	}
	c.window.generation++
	gen := c.window.generation
	c.window = window{
		status: status, adminFabric: req.FabricIndex, adminVendor: vendor, generation: gen,
		timer: time.AfterFunc(w.Timeout, func() {
			c.update(func() {
				if c.window.generation == gen {
					c.log.Info("Commissioning window timed out")
					c.closeWindow()
				}
			})
		}),
	}
	c.windowChanged()
	c.log.Info("Opened commissioning window", "basic", w.Basic, "timeout", w.Timeout, "fabric", req.FabricIndex)
	if c.hooks.OpenWindow != nil {
		c.hook(func() { c.hooks.OpenWindow(w) })
	}
}

// closeWindow closes an open commissioning window. Call it with the Node's lock.
func (c *Commissioning) closeWindow() {
	if c.window.status == windowClosed {
		return
	}
	if c.window.timer != nil {
		c.window.timer.Stop()
	}
	c.window = window{generation: c.window.generation + 1}
	c.windowChanged()
	c.hook(c.hooks.CloseWindow)
}

func (c *Commissioning) windowChanged() {
	for _, id := range []uint32{0, 1, 2} {
		c.admin.Changed(id)
	}
}

func (c *Commissioning) revokeCommissioning(_ *im.Request, _ *tlv.Reader) (*im.Response, error) {
	if c.failSafe.armed {
		c.expireFailSafe()
	}
	if c.window.status == windowClosed {
		return nil, im.ClusterStatus{Status: im.StatusFailure, Code: adminWindowNotOpen}
	}
	c.closeWindow()
	return nil, nil
}
