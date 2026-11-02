// Package node wires Matter's layers into a node that a commissioner can add: the transport,
// PASE and CASE, the Interaction Model serving a bridge with the commissioning clusters, the
// fabric table, test attestation, and mDNS. It opens the commissioning window, advertises the
// node, and keeps sessions, resumptions and advertisements in step with commissioning.
package node

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/setup"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// pbkdfIterations is the PBKDF2 iteration count of the node's own passcode verifier.
const pbkdfIterations = 1000

// fabricSessionGrace is how long a removed fabric's sessions outlive it, so that the
// RemoveFabric response still reaches an admin that removed its own fabric.
const fabricSessionGrace = 2 * time.Second

// Config configures a Node.
type Config struct {
	// DataDir holds the fabric table and the setup code. Losing it means re-commissioning.
	DataDir string
	// Port is the Matter UDP port; 0 means 5540.
	Port uint16
	// Info is what the node says of itself. Its vendor and product IDs are replaced by the test
	// attestation's, which they must match.
	Info clusters.Info
	// DeviceName is the DN key of the commissionable service.
	DeviceName string
	// MDNS configures the mDNS responder.
	MDNS mdns.Options
	Log  logr.Logger
}

// advertiser is what the node needs of an mDNS responder.
type advertiser interface {
	Add(ctx context.Context, svc mdns.Service) error
	Remove(svc mdns.Service) error
	Close() error
}

func listenMDNS(opts mdns.Options) (advertiser, error) { return mdns.Listen(opts) }

// Setup is the node's own setup passcode and discriminator, kept in the data directory so that
// its pairing code doesn't change.
type Setup struct {
	Passcode      uint32 `json:"passcode"`
	Discriminator uint16 `json:"discriminator"`
}

// Node is a Matter node serving a bridge.
type Node struct {
	cfg        Config
	log        logr.Logger
	setup      Setup
	salt       []byte
	verifier   mcrypto.Verifier
	table      *fabric.Table
	comm       *clusters.Commissioning
	bridge     *clusters.Bridge
	server     *im.Server
	resumption *securechannel.ResumptionStore

	tr   *transport.Transport
	mdns advertiser
	ctx  context.Context
	// listenMDNS opens the advertiser; tests replace it.
	listenMDNS func(mdns.Options) (advertiser, error)

	sessions sessionSet

	mu             sync.Mutex
	commissionable *mdns.Service           // advertised while a window is open
	operational    map[uint8]*mdns.Service // advertised per fabric
}

// New loads the node's state from cfg.DataDir and builds its bridge, with no bridged devices.
// Add them through Bridge before Run.
func New(cfg Config) (*Node, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("matter: node needs a data directory")
	}
	if cfg.Port == 0 {
		cfg.Port = transport.Port
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	n := &Node{
		cfg: cfg, log: cfg.Log, resumption: securechannel.NewResumptionStore(0),
		operational: map[uint8]*mdns.Service{}, listenMDNS: listenMDNS,
	}
	var err error
	if n.setup, err = loadSetup(cfg.DataDir); err != nil {
		return nil, err
	}
	n.salt = make([]byte, 32)
	_, _ = rand.Read(n.salt)
	if n.verifier, err = mcrypto.NewVerifier(n.setup.Passcode, n.salt, pbkdfIterations); err != nil {
		return nil, err
	}
	if n.table, err = fabric.Open(fabric.DirStore{Dir: cfg.DataDir}); err != nil {
		return nil, err
	}
	creds, err := attestation.Test()
	if err != nil {
		return nil, err
	}
	n.comm, err = clusters.NewCommissioning(clusters.CommissioningConfig{
		Fabrics: n.table, Attestation: creds, Log: cfg.Log.WithName("commissioning"),
		Hooks: clusters.Hooks{
			OpenWindow:    n.openWindowHook,
			CloseWindow:   n.closeWindow,
			FabricAdded:   n.advertiseFabric,
			FabricRemoved: n.fabricRemoved,
			EndPASE:       n.endPASE,
			Commissioned:  n.commissioned,
		},
	})
	if err != nil {
		return nil, err
	}
	info := cfg.Info
	info.VendorID, info.ProductID = attestation.TestVendorID, attestation.TestProductID
	n.cfg.Info = info
	n.bridge = clusters.NewBridge(info, n.comm.Clusters()...)
	n.server = &im.Server{Node: n.bridge.Node, Access: n.comm, Log: cfg.Log.WithName("im")}
	return n, nil
}

// Bridge returns the node's bridge, to add devices to.
func (n *Node) Bridge() *clusters.Bridge { return n.bridge }

// Fabrics returns the node's fabric table.
func (n *Node) Fabrics() *fabric.Table { return n.table }

// Payload returns the node's onboarding payload, for its QR and manual pairing codes.
func (n *Node) Payload() setup.Payload {
	return setup.Payload{
		VendorID: attestation.TestVendorID, ProductID: attestation.TestProductID,
		Flow: setup.FlowStandard, Discovery: setup.DiscoveryOnNetwork,
		Discriminator: n.setup.Discriminator, Passcode: n.setup.Passcode,
	}
}

// Run serves the node until ctx is done. It advertises the node on each fabric it's on, and
// opens the commissioning window if it's on none, or if openWindow is set.
func (n *Node) Run(ctx context.Context, openWindow bool) error {
	tr, err := transport.Listen(fmt.Sprintf("[::]:%d", n.cfg.Port), transport.Options{Log: n.log.WithName("transport")})
	if err != nil {
		return err
	}
	n.tr, n.ctx = tr, ctx
	opts := n.cfg.MDNS
	opts.Log = n.log.WithName("mdns")
	if n.mdns, err = n.listenMDNS(opts); err != nil {
		_ = tr.Serve(cancelled()) // closes the socket
		return err
	}
	defer func() { _ = n.mdns.Close() }()

	n.server.Register(tr)
	(&securechannel.CASEResponder{
		Transport: tr, Fabrics: n.table.CASE(), Resumption: n.resumption,
		OnSession: n.sessions.add, Log: n.log.WithName("case"),
	}).Register()
	tr.Handle(transport.SecureChannel, []uint8{securechannel.OpPBKDFParamRequest}, refusePASE)

	served := make(chan error, 1)
	go func() { served <- tr.Serve(ctx) }()

	for _, f := range n.table.Fabrics() {
		n.advertiseFabric(f.Index)
	}
	if n.table.Count() == 0 || openWindow {
		n.openWindow(clusters.Window{Basic: true})
	}
	return <-served
}

// openWindowHook opens a window that an administrator asked for.
func (n *Node) openWindowHook(w clusters.Window) {
	n.openWindow(w)
	n.log.Info("Opened commissioning window for an administrator", "basic", w.Basic, "timeout", w.Timeout)
}

// openWindow starts answering PASE with w's verifier, or the node's own for a basic window,
// and advertises the commissionable service.
func (n *Node) openWindow(w clusters.Window) {
	mode := mdns.CommissioningEnhanced
	if w.Basic {
		w.Verifier, w.Salt, w.Iterations, w.Discriminator = n.verifier, n.salt, pbkdfIterations, n.setup.Discriminator
		mode = mdns.CommissioningBasic
	}
	(&securechannel.Responder{
		Transport: n.tr, Verifier: w.Verifier, Salt: w.Salt, Iterations: int(w.Iterations),
		OnSession: n.sessions.add, Log: n.log.WithName("pase"),
	}).Register()

	c := mdns.Commissionable{
		Instance: mdns.NewInstanceName(), Discriminator: w.Discriminator,
		VendorID: attestation.TestVendorID, ProductID: attestation.TestProductID,
		Mode: mode, DeviceType: clusters.DeviceAggregator.ID, DeviceName: n.cfg.DeviceName,
		Port: n.cfg.Port,
	}
	svc, err := c.Service()
	if err != nil {
		n.log.Error(err, "Could not build the commissionable service")
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.commissionable != nil {
		_ = n.mdns.Remove(*n.commissionable)
		n.commissionable = nil
	}
	if err := n.mdns.Add(n.ctx, svc); err != nil {
		n.log.Error(err, "Could not advertise the commissionable service")
		return
	}
	n.commissionable = &svc
	n.log.Info("Opened commissioning window", "discriminator", w.Discriminator, "instance", svc.Instance)
}

// closeWindow stops answering PASE, and stops advertising the commissionable service.
func (n *Node) closeWindow() {
	n.tr.Handle(transport.SecureChannel, []uint8{securechannel.OpPBKDFParamRequest}, refusePASE)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.commissionable == nil {
		return
	}
	if err := n.mdns.Remove(*n.commissionable); err != nil {
		n.log.Error(err, "Could not stop advertising the commissionable service")
	}
	n.commissionable = nil
	n.log.Info("Closed commissioning window")
}

// refusePASE answers PASE while no commissioning window is open.
func refusePASE(ctx context.Context, ex *transport.Exchange, _ transport.Message) {
	defer ex.Close()
	_ = ex.Send(ctx, securechannel.StatusReport{
		General: securechannel.GeneralFailure, Protocol: transport.SecureChannel, Code: securechannel.CodeInvalidParameter,
	}.Message())
}

// advertiseFabric advertises the node's operational service on a fabric, which AddNOC may have
// just added.
func (n *Node) advertiseFabric(index uint8) {
	f, ok := n.table.Fabric(index)
	if !ok {
		return
	}
	svc := mdns.Operational{CompressedFabricID: f.CompressedFabricID, NodeID: f.NodeID, Port: n.cfg.Port}.Service()
	n.mu.Lock()
	defer n.mu.Unlock()
	if old := n.operational[index]; old != nil {
		if old.Instance == svc.Instance {
			return
		}
		_ = n.mdns.Remove(*old)
		delete(n.operational, index)
	}
	if err := n.mdns.Add(n.ctx, svc); err != nil {
		n.log.Error(err, "Could not advertise the operational service", "fabric", index)
		return
	}
	n.operational[index] = &svc
	n.log.Info("Advertised operational service", "fabric", index, "instance", svc.Instance)
}

// commissioned closes the initial window, which Commissioning doesn't track, and advertises the
// fabric (again, for an UpdateNOC that changed the node ID).
func (n *Node) commissioned(index uint8) {
	n.closeWindow()
	n.advertiseFabric(index)
}

// endPASE ends every PASE session.
func (n *Node) endPASE() {
	for _, s := range n.sessions.list() {
		if s.AuthMode() == transport.AuthPASE {
			n.tr.RemoveSession(s)
		}
	}
}

// fabricRemoved ends a fabric's sessions and resumptions, and its advertisement. A node left on
// no fabric opens its window again.
func (n *Node) fabricRemoved(index uint8) {
	n.resumption.RemoveFabric(index)
	var ending []*transport.Session
	for _, s := range n.sessions.list() {
		if s.FabricIndex() == index {
			ending = append(ending, s)
		}
	}
	time.AfterFunc(fabricSessionGrace, func() {
		for _, s := range ending {
			n.tr.RemoveSession(s)
		}
	})
	n.mu.Lock()
	if svc := n.operational[index]; svc != nil {
		if err := n.mdns.Remove(*svc); err != nil {
			n.log.Error(err, "Could not stop advertising the operational service", "fabric", index)
		}
		delete(n.operational, index)
	}
	reopen := n.commissionable == nil && n.table.Count() == 0
	n.mu.Unlock()
	n.log.Info("Ended fabric's sessions", "fabric", index, "sessions", len(ending))
	if reopen {
		n.openWindow(clusters.Window{Basic: true})
	}
}

// sessionSet is the secure sessions PASE and CASE established, until each closes.
type sessionSet struct {
	mu  sync.Mutex
	set map[*transport.Session]struct{}
}

func (s *sessionSet) add(sess *transport.Session) {
	s.mu.Lock()
	if s.set == nil {
		s.set = map[*transport.Session]struct{}{}
	}
	s.set[sess] = struct{}{}
	s.mu.Unlock()
	go func() {
		<-sess.Done()
		s.mu.Lock()
		delete(s.set, sess)
		s.mu.Unlock()
	}()
}

func (s *sessionSet) list() []*transport.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := make([]*transport.Session, 0, len(s.set))
	for sess := range s.set {
		l = append(l, sess)
	}
	return l
}

const setupFile = "setup.json"

// loadSetup reads the setup code from dir, or makes and saves a random one.
func loadSetup(dir string) (Setup, error) {
	path := filepath.Join(dir, setupFile)
	b, err := os.ReadFile(path)
	if err == nil {
		var s Setup
		if err := json.Unmarshal(b, &s); err != nil {
			return Setup{}, fmt.Errorf("matter: reading %s: %w", path, err)
		}
		if !setup.ValidPasscode(s.Passcode) || s.Discriminator > 0xFFF {
			return Setup{}, fmt.Errorf("matter: %s has an invalid passcode or discriminator", path)
		}
		return s, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Setup{}, err
	}
	var s Setup
	for !setup.ValidPasscode(s.Passcode) {
		s.Passcode = randomUint32() % 100000000
	}
	s.Discriminator = uint16(randomUint32() & 0xFFF)
	if b, err = json.Marshal(s); err != nil {
		return Setup{}, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return Setup{}, err
	}
	return s, nil
}

func randomUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

// cancelled returns a context that's already done.
func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Interfaces returns the interfaces named, for Config.MDNS.
func Interfaces(names []string) ([]net.Interface, error) {
	var ifs []net.Interface
	for _, name := range names {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			return nil, err
		}
		ifs = append(ifs, *ifi)
	}
	return ifs, nil
}
