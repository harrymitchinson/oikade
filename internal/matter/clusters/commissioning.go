package clusters

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Commissioning cluster IDs.
const (
	IDGeneralCommissioning       uint32 = 0x0030
	IDNetworkCommissioning       uint32 = 0x0031
	IDAccessControl              uint32 = 0x001F
	IDAdministratorCommissioning uint32 = 0x003C
	IDOperationalCredentials     uint32 = 0x003E
	IDGroupKeyManagement         uint32 = 0x003F
)

// The fail-safe's limits (General Commissioning, BasicCommissioningInfo).
const (
	failSafeExpiry        = 60 * time.Second
	maxCumulativeFailSafe = 900 * time.Second
)

// CommissioningErrorEnum.
const (
	commissioningOK            = 0
	commissioningValueOutside  = 1
	commissioningInvalidAuth   = 2
	commissioningNoFailSafe    = 3
	commissioningBusyWithOther = 4
)

// Hooks are what commissioning asks of the rest of the node. Each is optional. They're called
// after the change, one at a time in the order of the changes, outside the Node's lock, so they
// may use the Node and the Transport.
type Hooks struct {
	// OpenWindow starts accepting PASE with w and advertising it, for Administrator
	// Commissioning; CloseWindow stops.
	OpenWindow  func(w Window)
	CloseWindow func()
	// FabricAdded is called when AddNOC adds a fabric, still pending until CommissioningComplete:
	// advertise the node's operational service on it. The commissioner finds the node by that
	// service to establish the CASE session it completes commissioning over.
	FabricAdded func(index uint8)
	// FabricRemoved is called when a fabric is removed, or a pending one rolled back: end its
	// sessions and subscriptions.
	FabricRemoved func(index uint8)
	// EndPASE is called when commissioning ends, completed or rolled back: end every PASE
	// session. A PASE session holds Administer for as long as it lasts, so a node that doesn't
	// end them leaves the commissioner in charge.
	EndPASE func()
	// FabricUpdated is called when UpdateNOC changes a fabric's NOC, still pending until
	// CommissioningComplete, and when the fail-safe rolls that back: the node ID may have changed,
	// so advertise the operational service again.
	FabricUpdated func(index uint8)
	// Commissioned is called when CommissioningComplete commits a fabric or NOC update.
	Commissioned func(index uint8)
}

// CommissioningConfig configures the commissioning clusters.
type CommissioningConfig struct {
	Fabrics     *fabric.Table
	Attestation *attestation.Credentials
	Hooks       Hooks
	Log         *slog.Logger
}

// Commissioning is the root endpoint's commissioning clusters (Core spec, chapter 11): General
// Commissioning with the fail-safe, Operational Credentials, Network Commissioning for Ethernet,
// Access Control, Group Key Management, and Administrator Commissioning. It's the Server's
// AccessControl, from the fabric table's entries.
type Commissioning struct {
	fabrics *fabric.Table
	creds   *attestation.Credentials
	hooks   Hooks
	log     *slog.Logger

	general, opCreds, network, acl, groupKeys, admin *im.Cluster

	breadcrumb uint64
	regulatory uint64
	failSafe   failSafe
	window     window

	queue hookQueue
}

// hookQueue runs hooks one at a time, in order, in a goroutine that lives while there are any.
type hookQueue struct {
	mu      sync.Mutex
	pending []func()
	running bool
}

func (q *hookQueue) push(f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, f)
	if !q.running {
		q.running = true
		go q.run()
	}
}

func (q *hookQueue) run() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		f := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		f()
	}
}

// failSafe is the fail-safe context (Core spec, 11.10.7.2). The Node's lock guards it.
type failSafe struct {
	armed       bool
	fabricIndex uint8 // the accessing fabric that armed it, or that AddNOC added
	session     *transport.Session
	started     time.Time // when it was first armed, for the cumulative limit
	timer       *time.Timer
	generation  int // which timer is current

	csrForUpdate bool
	csr          bool
	rootAdded    bool
	nocAdded     bool
	nocUpdated   bool
}

// NewCommissioning returns the commissioning clusters. Pass Clusters to NewBridge, and the
// Commissioning to the Server as its Access.
func NewCommissioning(cfg CommissioningConfig) (*Commissioning, error) {
	if cfg.Fabrics == nil || cfg.Attestation == nil {
		return nil, errors.New("matter: commissioning needs a fabric table and attestation credentials")
	}
	c := &Commissioning{fabrics: cfg.Fabrics, creds: cfg.Attestation, hooks: cfg.Hooks, log: logging.OrDiscard(cfg.Log)}
	c.general = c.generalCommissioning()
	c.opCreds = c.operationalCredentials()
	c.network = networkCommissioning()
	c.acl = c.accessControl()
	c.groupKeys = c.groupKeyManagement()
	c.admin = c.administratorCommissioning()
	for _, cl := range c.Clusters() {
		critical(cl)
	}
	return c, nil
}

// Clusters returns the clusters, for the root endpoint.
func (c *Commissioning) Clusters() []*im.Cluster {
	return []*im.Cluster{c.general, c.opCreds, c.network, c.acl, c.groupKeys, c.admin}
}

// critical makes a panic in any of the cluster's commands or writes stop the host, even though it runs
// on a handler, where a panic is contained: it would leave a window, a fail-safe or the fabrics half
// moved.
func critical(cl *im.Cluster) {
	for i := range cl.Commands {
		invoke := cl.Commands[i].Invoke
		if invoke == nil {
			continue
		}
		cl.Commands[i].Invoke = func(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
			defer contain.Critical()
			return invoke(req, fields)
		}
	}
	for i := range cl.Attributes {
		write := cl.Attributes[i].Write
		if write == nil {
			continue
		}
		cl.Attributes[i].Write = func(req *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
			defer contain.Critical()
			return write(req, r, op)
		}
	}
}

// update runs f under the Node's lock, from a timer.
func (c *Commissioning) update(f func()) {
	if n := c.general.Node(); n != nil {
		n.Update(f)
	}
}

func (c *Commissioning) hook(f func()) {
	if f != nil {
		c.queue.push(f)
	}
}

func (c *Commissioning) generalCommissioning() *im.Cluster {
	admin := im.PrivilegeAdminister
	cl := &im.Cluster{
		ID: IDGeneralCommissioning, Revision: 1,
		Attributes: []im.Attribute{
			{
				ID:   0x0000, // Breadcrumb
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutUint(tag, c.breadcrumb); return nil },
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Uint()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					c.setBreadcrumb(v)
					return im.StatusSuccess
				},
				WritePrivilege: admin,
			},
			readOnly(0x0001, func(w *tlv.Writer, tag tlv.Tag) { // BasicCommissioningInfo
				w.StartStructure(tag)
				w.PutUint(tlv.Context(0), uint64(failSafeExpiry/time.Second))
				w.PutUint(tlv.Context(1), uint64(maxCumulativeFailSafe/time.Second))
				w.End()
			}),
			readOnly(0x0002, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, c.regulatory) }), // RegulatoryConfig
			uintAttr(0x0003, 2), // LocationCapability: indoor and outdoor
			readOnly(0x0004, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) }), // SupportsConcurrentConnection
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: c.armFailSafe, Privilege: admin},
			{ID: 0x02, Invoke: c.setRegulatoryConfig, Privilege: admin},
			{ID: 0x04, Invoke: c.commissioningComplete, Privilege: admin},
		},
		Generated: []uint32{0x01, 0x03, 0x05},
	}
	return cl
}

func (c *Commissioning) setBreadcrumb(v uint64) {
	if v != c.breadcrumb {
		c.breadcrumb = v
		c.general.Changed(0x0000)
	}
}

// commissioningResponse is ArmFailSafeResponse, SetRegulatoryConfigResponse or
// CommissioningCompleteResponse.
func commissioningResponse(command uint32, code uint8) *im.Response {
	return &im.Response{Command: command, Fields: func(w *tlv.Writer) {
		w.PutUint(tlv.Context(0), uint64(code))
		w.PutUTF8(tlv.Context(1), "")
	}}
}

func (c *Commissioning) armFailSafe(req *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var expiry, breadcrumb uint64
	var seen [2]bool
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			expiry, err = r.Uint()
			seen[0] = true
		case 1:
			breadcrumb, err = r.Uint()
			seen[1] = true
		}
		return err
	}); err != nil || seen != [2]bool{true, true} || expiry > 0xFFFF {
		return nil, im.StatusInvalidCommand
	}
	const resp = 0x01
	fs := &c.failSafe
	if fs.armed && fs.fabricIndex != req.FabricIndex {
		return commissioningResponse(resp, commissioningBusyWithOther), nil
	}
	if !fs.armed && c.window.status != windowClosed && req.AuthMode == transport.AuthCASE {
		return commissioningResponse(resp, commissioningBusyWithOther), nil
	}
	if expiry == 0 {
		if fs.armed {
			c.expireFailSafe()
		}
		c.setBreadcrumb(breadcrumb)
		return commissioningResponse(resp, commissioningOK), nil
	}
	now := time.Now()
	if !fs.armed {
		*fs = failSafe{armed: true, fabricIndex: req.FabricIndex, session: req.Session, started: now, generation: fs.generation}
	}
	d := time.Duration(expiry) * time.Second
	if now.Add(d).After(fs.started.Add(maxCumulativeFailSafe)) {
		d = time.Until(fs.started.Add(maxCumulativeFailSafe))
	}
	c.armTimer(d)
	c.setBreadcrumb(breadcrumb)
	return commissioningResponse(resp, commissioningOK), nil
}

// armTimer (re)starts the fail-safe's timer. Call it with the Node's lock.
func (c *Commissioning) armTimer(d time.Duration) {
	fs := &c.failSafe
	if fs.timer != nil {
		fs.timer.Stop()
	}
	fs.generation++
	gen := fs.generation
	fs.timer = time.AfterFunc(d, func() {
		c.update(func() {
			if c.failSafe.armed && c.failSafe.generation == gen {
				c.log.Info("Fail-safe expired: rolling back")
				c.expireFailSafe()
			}
		})
	})
}

// expireFailSafe rolls back what the fail-safe guarded (Core spec, 11.10.7.2.2). Call it with
// the Node's lock.
func (c *Commissioning) expireFailSafe() {
	fs := &c.failSafe
	if fs.timer != nil {
		fs.timer.Stop()
	}
	added := c.fabrics.Revert()
	updated := uint8(0)
	if fs.nocUpdated {
		updated = fs.fabricIndex
	}
	if fs.session != nil && fs.session.AuthMode() == transport.AuthPASE {
		fs.session.SetFabricIndex(0)
	}
	*fs = failSafe{generation: fs.generation + 1}
	c.setBreadcrumb(0)
	c.fabricsChanged()
	if added != 0 && c.hooks.FabricRemoved != nil {
		c.hook(func() { c.hooks.FabricRemoved(added) })
	}
	if updated != 0 && c.hooks.FabricUpdated != nil {
		c.hook(func() { c.hooks.FabricUpdated(updated) })
	}
	c.hook(c.hooks.EndPASE)
}

// disarm ends the fail-safe without rolling back. Call it with the Node's lock.
func (c *Commissioning) disarm() {
	fs := &c.failSafe
	if fs.timer != nil {
		fs.timer.Stop()
	}
	*fs = failSafe{generation: fs.generation + 1}
}

func (c *Commissioning) setRegulatoryConfig(_ *im.Request, fields *tlv.Reader) (*im.Response, error) {
	var config, breadcrumb uint64
	var country string
	var seen [3]bool
	if err := readFields(fields, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			config, err = r.Uint()
		case 1:
			country, err = r.UTF8()
		case 2:
			breadcrumb, err = r.Uint()
		default:
			return nil
		}
		seen[tag] = true
		return err
	}); err != nil || seen != [3]bool{true, true, true} {
		return nil, im.StatusInvalidCommand
	}
	if len(country) != 2 {
		return nil, im.StatusConstraintError
	}
	if config > 2 {
		return commissioningResponse(0x03, commissioningValueOutside), nil
	}
	if config != c.regulatory {
		c.regulatory = config
		c.general.Changed(0x0002)
	}
	c.setBreadcrumb(breadcrumb)
	return commissioningResponse(0x03, commissioningOK), nil
}

func (c *Commissioning) commissioningComplete(req *im.Request, _ *tlv.Reader) (*im.Response, error) {
	const resp = 0x05
	fs := &c.failSafe
	if !fs.armed {
		return commissioningResponse(resp, commissioningNoFailSafe), nil
	}
	if req.AuthMode != transport.AuthCASE || req.FabricIndex == 0 || req.FabricIndex != fs.fabricIndex {
		return commissioningResponse(resp, commissioningInvalidAuth), nil
	}
	index, added, updated := c.fabrics.Pending()
	if err := c.fabrics.Commit(); err != nil {
		c.log.Error("Could not save the fabric table", "err", err)
		return nil, im.StatusFailure
	}
	c.disarm()
	c.setBreadcrumb(0)
	c.closeWindow()
	c.fabricsChanged()
	c.hook(c.hooks.EndPASE)
	if added || updated {
		c.log.Info("Commissioned", "fabric", index)
		if c.hooks.Commissioned != nil {
			c.hook(func() { c.hooks.Commissioned(index) })
		}
	}
	return commissioningResponse(resp, commissioningOK), nil
}

// readFields reads a command's fields, which are nil when it has none.
func readFields(r *tlv.Reader, field func(r *tlv.Reader, tag uint8) error) error {
	if r == nil {
		return nil
	}
	return im.Fields(r, field)
}

// networkCommissioning is Network Commissioning for an Ethernet interface the node is already
// connected through.
func networkCommissioning() *im.Cluster {
	const featureEthernet = 1 << 2
	enabled := true
	var cl *im.Cluster
	cl = &im.Cluster{
		ID: IDNetworkCommissioning, Revision: 1, FeatureMap: featureEthernet,
		Attributes: []im.Attribute{
			uintAttr(0x0000, 1), // MaxNetworks
			readOnly(0x0001, func(w *tlv.Writer, tag tlv.Tag) { // Networks
				w.StartArray(tag)
				w.StartStructure(tlv.Anonymous)
				w.PutBytes(tlv.Context(0), []byte("eth0"))
				w.PutBool(tlv.Context(1), true)
				w.End()
				w.End()
			}),
			{
				ID:   0x0004, // InterfaceEnabled
				Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { w.PutBool(tag, enabled); return nil },
				Write: func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
					v, err := r.Bool()
					if err != nil || op != im.Replace {
						return im.StatusInvalidDataType
					}
					if v != enabled {
						enabled = v
						cl.Changed(0x0004)
					}
					return im.StatusSuccess
				},
				WritePrivilege: im.PrivilegeAdminister,
			},
			readOnly(0x0005, func(w *tlv.Writer, tag tlv.Tag) { w.PutNull(tag) }), // LastNetworkingStatus
			readOnly(0x0006, func(w *tlv.Writer, tag tlv.Tag) { w.PutNull(tag) }), // LastNetworkID
			readOnly(0x0007, func(w *tlv.Writer, tag tlv.Tag) { w.PutNull(tag) }), // LastConnectErrorValue
		},
	}
	return cl
}
