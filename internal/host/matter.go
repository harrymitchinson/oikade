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
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
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
	// matterRetry is how soon a bridge that couldn't take a device tries again.
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
	serving atomic.Bool
	// broken is why the bridge can't serve at all.
	broken error
}

// openMatter makes the bridge, loading what must survive. It serves nothing until run.
func (h *Host) openMatter(data *store.Store) (*matterBridge, error) {
	if h.Matter == nil {
		return nil, nil
	}
	if len(h.Matter.Name) > 32 {
		return nil, errors.New("a Matter bridge's name is at most 32 bytes")
	}
	values := data.Namespace(matterNamespace)
	id, err := loadIdentity(values)
	if err != nil {
		return nil, err
	}
	creds, err := attestation.WithDAC(id.DAC, id.Key)
	if err != nil {
		return nil, fmt.Errorf("reading the Matter attestation: %w", err)
	}
	log := h.Log.WithName("matter")
	port := h.Matter.Port
	if port == 0 {
		port = DefaultMatterPort
	}
	m := &matterBridge{log: log, port: port, id: id, changes: make(chan struct{}, 1)}
	cfg := node.Config{
		Fabrics: fabricStore{values}, Setup: node.Setup{Passcode: id.Passcode, Discriminator: id.Discriminator},
		Attestation: creds, Port: port, DeviceName: h.Matter.Name, Log: log,
		Listening: func() {
			m.serving.Store(true)
			log.Info("Started Matter bridge", "port", port, "fabrics", m.node.Fabrics().Count())
		},
	}
	if len(h.Matter.Interfaces) > 0 {
		// An interface that isn't up yet is the same as a taken port: not worth stopping a home for.
		list, err := node.Interfaces(h.Matter.Interfaces)
		m.broken = err
		cfg.MDNS = mdns.Options{Interfaces: list}
	}
	cfg.Info.UniqueID = id.UniqueID
	cfg.Info.VendorName, cfg.Info.ProductName, cfg.Info.NodeLabel = "Oikade", "Oikade bridge", h.Matter.Name
	cfg.Info.SoftwareVersion, cfg.Info.SoftwareVersionString = 1, h.Matter.Version
	if m.node, err = node.New(cfg); err != nil {
		return nil, fmt.Errorf("opening the Matter bridge: %w", err)
	}
	m.Bridge = &matterbridge.Bridge{Matter: m.node.Bridge()}
	return m, nil
}

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

// run serves the bridge until ctx ends. A bridge that can't start is worth saying loudly, and not
// worth stopping a home for: automations don't need it.
func (m *matterBridge) run(ctx context.Context) {
	if m.broken != nil {
		m.log.Error(m.broken, "Matter bridge stopped")
		return
	}
	m.Writes.Start(logr.NewContext(ctx, m.log))
	retry := m.apply()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer m.serving.Store(false)
		if err := m.node.Run(ctx); err != nil && ctx.Err() == nil {
			m.log.Error(err, "Matter bridge stopped")
		}
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
	payload := m.node.Payload()
	manual, err := payload.ManualCode()
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	qr, err := payload.QRCode()
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	out := &api.Matter{
		Serving: m.serving.Load(), Port: m.port, ManualCode: manual, QRCode: qr, Passcode: m.id.Passcode,
		Window: m.node.WindowOpen(), Fabrics: []api.Fabric{},
	}
	for _, f := range m.node.Fabrics().Fabrics() {
		out.Fabrics = append(out.Fabrics, api.Fabric{Index: f.Index, VendorID: f.VendorID, Label: f.Label})
	}
	return api.Response{Matter: out}
}
