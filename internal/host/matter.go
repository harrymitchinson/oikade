package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/node"
	"github.com/harrymitchinson/oikade/internal/matter/setup"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
	"github.com/harrymitchinson/oikade/internal/matterbridge"
	"github.com/harrymitchinson/oikade/internal/store"
)

// DefaultMatterPort is Matter's own UDP port.
const DefaultMatterPort = transport.Port

const (
	// matterNamespace is where the store keeps the bridge's identity and fabrics.
	matterNamespace = "matter"
	identityKey     = "identity"
	fabricsKey      = "fabrics"
	// matterWriteTimeout bounds one write from a controller.
	matterWriteTimeout = 5 * time.Second
	// matterRetry is how soon a bridge that couldn't start, or take a device, tries again.
	matterRetry = time.Minute
)

// Matter configures the bridge a host serves.
type Matter struct {
	// Port is its UDP port. Zero is Matter's own, 5540.
	Port uint16
	// Name is its NodeLabel, at most 32 bytes.
	Name string
	// Interfaces are where it advertises over mDNS. Empty means every multicast interface.
	Interfaces []string
	// Version is the software version it reports.
	Version string
	// Retry is how soon it tries again after failing. Zero is a minute.
	Retry time.Duration
}

// identity is what makes the bridge the same bridge to a controller. Losing any of it means pairing
// again, so it's written once, before anything can be paired with it.
type identity struct {
	Passcode      uint32 `json:"passcode"`
	Discriminator uint16 `json:"discriminator"`
	UniqueID      string `json:"uniqueID"`
	// DAC and Key are its attestation certificate and private key, in PEM.
	DAC []byte `json:"dac"`
	Key []byte `json:"key"`
}

// loadIdentity reads the bridge's identity, or makes one if it never had one. An unreadable one is an
// error rather than a new one, which would quietly make every controller lose the bridge.
func loadIdentity(values *store.Namespace) (identity, error) {
	var id identity
	raw, ok, err := values.Get(identityKey)
	if err != nil {
		return id, fmt.Errorf("reading the Matter identity: %w", err)
	}
	if ok {
		if err := json.Unmarshal(raw, &id); err != nil {
			return id, fmt.Errorf("reading the Matter identity: %w", err)
		}
		return id, nil
	}
	id.Passcode, id.Discriminator = setup.Random()
	unique := make([]byte, 16)
	_, _ = rand.Read(unique)
	id.UniqueID = strings.ToUpper(hex.EncodeToString(unique))
	if id.DAC, id.Key, err = attestation.TestDACPEM(); err != nil {
		return id, err
	}
	if raw, err = json.Marshal(id); err != nil {
		return id, err
	}
	if err := values.Put(identityKey, raw); err != nil {
		return id, fmt.Errorf("saving the Matter identity: %w", err)
	}
	return id, nil
}

// fabricStore keeps the fabric table in the store. A Put is on disk when it returns, so a fabric is
// saved before commissioning is told it worked.
type fabricStore struct{ values *store.Namespace }

// Load implements fabric.Store.
func (f fabricStore) Load() ([]byte, error) {
	raw, _, err := f.values.Get(fabricsKey)
	return raw, err
}

// Save implements fabric.Store.
func (f fabricStore) Save(b []byte) error { return f.values.Put(fabricsKey, b) }

// matterBridge is the Matter bridge: it lives in the host so that nothing your code does reaches a
// controller. It's a driver.Sink, so it follows devices without your process.
type matterBridge struct {
	*matterbridge.Bridge
	log     logr.Logger
	node    *node.Node
	port    uint16
	id      identity
	devices *hub.Store
	known   *inventory
	changes chan struct{}
	retry   time.Duration
	serving atomic.Bool
	// broken is why the bridge can never serve until someone mends its state. It's set before run,
	// and such a bridge has no node.
	broken error
	// down is why it isn't serving now, which trying again may mend.
	down atomic.Pointer[string]
}

// openMatter makes the bridge, serving nothing until run. State it can't read leaves it broken rather
// than making new state, which would quietly lose every controller: that's the home's Matter gone,
// and not worth its automations too.
func (h *Host) openMatter(data *store.Store) (*matterBridge, error) {
	if h.Matter == nil {
		return nil, nil
	}
	if len(h.Matter.Name) > 32 {
		return nil, errors.New("a Matter bridge's name is at most 32 bytes")
	}
	m := &matterBridge{log: h.Log.WithName("matter"), port: h.Matter.Port, retry: h.Matter.Retry, changes: make(chan struct{}, 1)}
	if m.port == 0 {
		m.port = DefaultMatterPort
	}
	if m.retry == 0 {
		m.retry = matterRetry
	}
	if err := m.open(h.Matter, data.Namespace(matterNamespace)); err != nil {
		m.broken, m.node = err, nil
		m.log.Error(err, "Matter bridge stopped")
	}
	return m, nil
}

// open loads the bridge's identity and fabrics.
func (m *matterBridge) open(c *Matter, values *store.Namespace) error {
	var err error
	if m.id, err = loadIdentity(values); err != nil {
		return err
	}
	creds, err := attestation.WithDAC(m.id.DAC, m.id.Key)
	if err != nil {
		return fmt.Errorf("reading the Matter attestation: %w", err)
	}
	cfg := node.Config{
		Fabrics: fabricStore{values}, Setup: node.Setup{Passcode: m.id.Passcode, Discriminator: m.id.Discriminator},
		Attestation: creds, Port: m.port, DeviceName: c.Name, Interfaces: c.Interfaces, Log: m.log,
		Listening: func() {
			m.serving.Store(true)
			m.down.Store(nil)
			m.log.Info("Started Matter bridge", "port", m.port, "fabrics", m.node.Fabrics().Count())
		},
	}
	cfg.Info.UniqueID = m.id.UniqueID
	cfg.Info.VendorName, cfg.Info.ProductName, cfg.Info.NodeLabel = "Oikade", "Oikade bridge", c.Name
	cfg.Info.SoftwareVersion, cfg.Info.SoftwareVersionString = 1, c.Version
	if m.node, err = node.New(cfg); err != nil {
		return fmt.Errorf("opening the Matter bridge: %w", err)
	}
	m.Bridge = &matterbridge.Bridge{Matter: m.node.Bridge()}
	return nil
}

// working is whether the bridge has a node to serve, so that there's anything to connect or run.
func (m *matterBridge) working() bool { return m != nil && m.broken == nil }

// connect gives the bridge what it needs of the host, which needs the bridge to exist first.
func (m *matterBridge) connect(devices *hub.Store, known *inventory, eng *engine.Engine, setter driver.Setter) {
	m.devices, m.known, m.Store = devices, known, devices
	known.changed = m.changed
	m.Writes = &matterbridge.Coalescer{
		Setter: &matterWrites{engine: eng, devices: devices, setter: setter, restore: m.Restore},
		Current: func(dev types.NamespacedName, name string) (any, bool) {
			snap, ok := devices.Get(dev)
			if !ok {
				return nil, false
			}
			v, ok := snap.Values[name]
			return v, ok
		},
		Timeout: matterWriteTimeout,
	}
}

// changed asks run to look at the devices again. It doesn't block.
func (m *matterBridge) changed() {
	select {
	case m.changes <- struct{}{}:
	default:
	}
}

// Discovered implements driver.Sink: a device's first report makes it reachable.
func (m *matterBridge) Discovered(types.NamespacedName, driver.Info) { m.changed() }

// Reassigned implements hub.Reassigner: a device's state changed with no driver event.
func (m *matterBridge) Reassigned(types.NamespacedName) { m.changed() }

// want is what should be on the bridge. A device on record stays there while its driver is away, so
// controllers don't see it vanish, and only reserved and removed ones are left out.
func (m *matterBridge) want() map[uint16]matterbridge.Device {
	out := map[uint16]matterbridge.Device{}
	for _, d := range m.known.registry.List().Devices {
		if d.Removed || d.ID == "" || !matterbridge.Supported(d.Capabilities) {
			continue
		}
		dev := key(d.Name)
		snap, live := m.devices.Get(dev)
		out[d.Endpoint] = matterbridge.Device{
			Key: dev, Endpoint: d.Endpoint, Name: d.Name, Capabilities: d.Capabilities,
			// A name can move, and an endpoint can't, so a controller keeps its device through either.
			Unique: matterbridge.UniqueID(types.NamespacedName{Namespace: m.id.UniqueID, Name: strconv.Itoa(int(d.Endpoint))}),
			Absent: !live || !snap.Discovered,
		}
	}
	return out
}

// apply brings the bridge up to date, and says when to try again if it couldn't.
func (m *matterBridge) apply() <-chan time.Time {
	if err := m.Apply(m.log, m.want()); err != nil {
		m.log.Error(err, "Failed to update the Matter bridge")
		return time.After(matterRetry)
	}
	return nil
}

// run serves the bridge until ctx ends.
func (m *matterBridge) run(ctx context.Context) {
	m.Writes.Start(logr.NewContext(ctx, m.log))
	retry := m.apply()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.serve(ctx)
	}()
	for {
		select {
		case <-done:
			return
		case <-m.changes:
			retry = m.apply()
		case <-retry:
			retry = m.apply()
		}
	}
}

// serve runs the node, and starts it again whenever it stops. A port that's taken, or an interface
// that isn't up yet, is worth saying loudly and not worth stopping a home for: automations don't
// need the bridge.
func (m *matterBridge) serve(ctx context.Context) {
	for {
		err := m.node.Run(ctx)
		m.serving.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the node stopped")
		}
		why := err.Error()
		m.down.Store(&why)
		m.log.Error(err, "Matter bridge stopped", "retry", m.retry.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.retry):
		}
	}
}

// matterWrites is how a controller's write reaches a device: through arbitration, as on the bus
// before. The write doesn't wait for your process, and a Safety claim refuses it, because a valve
// shouldn't open for the moment it takes to write it back.
type matterWrites struct {
	engine  *engine.Engine
	devices *hub.Store
	setter  driver.Setter
	// restore takes the clusters back after a write that didn't happen.
	restore func(dev types.NamespacedName, name string, otherwise any)
}

// Set implements driver.Setter.
func (w *matterWrites) Set(ctx context.Context, dev types.NamespacedName, name string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if holder, wants := heldAtSafety(w.engine, w.devices, dev, name, value); holder != "" {
		w.restore(dev, name, wants)
		return fmt.Errorf("%s holds it at Safety", holder)
	}
	if snap, ok := w.devices.Get(dev); ok && (snap.Online == nil || *snap.Online) {
		w.engine.MatterWrote(dev, name, value)
	}
	err := w.setter.Set(ctx, dev, name, value)
	if err != nil {
		w.engine.ForgetMatterWrite(dev, name)
		w.restore(dev, name, nil)
	}
	return err
}

// matter answers oikade matter.
func (s *apiServer) matter() api.Response {
	m := s.bridge
	if m == nil {
		return api.Response{Error: "this host has Matter turned off"}
	}
	out := &api.Matter{Port: m.port, Fabrics: []api.Fabric{}}
	if !m.working() {
		out.Problem = m.broken.Error()
		return api.Response{Matter: out}
	}
	if why := m.down.Load(); why != nil {
		out.Problem = *why
	}
	payload := m.node.Payload()
	var err error
	if out.ManualCode, err = payload.ManualCode(); err != nil {
		return api.Response{Error: err.Error()}
	}
	if out.QRCode, err = payload.QRCode(); err != nil {
		return api.Response{Error: err.Error()}
	}
	out.Serving, out.Passcode, out.Window = m.serving.Load(), m.id.Passcode, m.node.WindowOpen()
	for _, f := range m.node.Fabrics().Fabrics() {
		out.Fabrics = append(out.Fabrics, api.Fabric{Index: f.Index, VendorID: f.VendorID, Label: f.Label})
	}
	return api.Response{Matter: out}
}
