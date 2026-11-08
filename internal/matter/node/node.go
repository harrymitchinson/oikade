// Package node wires Matter's layers into a node that a commissioner can add: the transport,
// PASE and CASE, the Interaction Model serving a bridge with the commissioning clusters, the
// fabric table, test attestation, and mDNS. It opens the commissioning window, advertises the
// node, and keeps sessions, resumptions and advertisements in step with commissioning.
package node

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
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

// closeSessionsWait is how long saying goodbye to controllers may take when the node stops.
const closeSessionsWait = time.Second

// fabricSessionGrace is how long a removed fabric's sessions outlive it, so that the
// RemoveFabric response still reaches an admin that removed its own fabric.
const fabricSessionGrace = 2 * time.Second

// The initial commissioning window's limits (Core spec, 5.4.2.3.1). DefaultWindowTimeout, the
// longest, is how long it stays open unless configured.
const (
	MinWindowTimeout     = 3 * time.Minute
	DefaultWindowTimeout = 15 * time.Minute
)

// Config configures a Node.
type Config struct {
	// DataDir holds the fabric table, unless Fabrics does. Losing it means re-commissioning.
	DataDir string
	// Fabrics, if set, keeps the fabric table, and DataDir isn't used.
	Fabrics fabric.Store
	// Setup is the node's own passcode and discriminator. It must stay the same across
	// restarts, or the pairing code changes.
	Setup Setup
	// Attestation is what the node attests with; nil means connectedhomeip's test credentials.
	Attestation *attestation.Credentials
	// WindowTimeout is how long the initial commissioning window stays open; 0 means
	// DefaultWindowTimeout.
	WindowTimeout time.Duration
	// Interfaces name where to advertise, instead of MDNS.Interfaces. Each Run looks them up, so
	// one that comes up late is found by the next Run.
	Interfaces []string
	// Port is the Matter UDP port; 0 means 5540.
	Port uint16
	// Info is what the node says of itself. Its vendor and product IDs are replaced by the test
	// attestation's, which they must match.
	Info clusters.Info
	// DeviceName is the DN key of the commissionable service.
	DeviceName string
	// MDNS configures the mDNS responder.
	MDNS mdns.Options
	// FabricsChanged, if set, is called after a fabric is committed or removed. It runs in the
	// order of the changes, and may block briefly.
	FabricsChanged func()
	// Listening, if set, is called once the node's sockets are open.
	Listening func()
	// Observer, if set, hears what controllers do, for metrics.
	Observer im.Observer
	Log      *slog.Logger
}

// advertiser is what the node needs of an mDNS responder.
type advertiser interface {
	Add(ctx context.Context, svc mdns.Service) error
	Remove(svc mdns.Service) error
	Close() error
}

func listenMDNS(opts mdns.Options) (advertiser, error) { return mdns.Listen(opts) }

// Setup is the node's own setup passcode and discriminator.
type Setup struct {
	Passcode      uint32
	Discriminator uint16
}

// Valid reports whether s may be a node's setup code.
func (s Setup) Valid() bool {
	return setup.ValidPasscode(s.Passcode) && s.Discriminator <= setup.MaxDiscriminator
}

// Node is a Matter node serving a bridge.
type Node struct {
	cfg        Config
	log        *slog.Logger
	salt       []byte
	verifier   mcrypto.Verifier
	table      *fabric.Table
	comm       *clusters.Commissioning
	bridge     *clusters.Bridge
	server     *im.Server
	resumption *securechannel.ResumptionStore

	// run is the latest Run's, which hooks and timers may be reaching for as the next one starts.
	run atomic.Pointer[running]
	// listenMDNS opens the advertiser; tests replace it.
	listenMDNS func(mdns.Options) (advertiser, error)

	sessions sessionSet

	// mu guards the advertisements. It's never held while the responder probes, which takes
	// about a second.
	mu             sync.Mutex
	window         *clusters.Window        // open, for a later Run to take on
	commissionable *mdns.Service           // advertised while a window is open
	operational    map[uint8]*mdns.Service // advertised per fabric
}

// running is what one Run serves with.
type running struct {
	tr   *transport.Transport
	mdns advertiser
	ctx  context.Context
}

// New loads the node's fabric table and builds its bridge, with no bridged devices. Add them through
// Bridge before Run.
func New(cfg Config) (*Node, error) {
	cfg.Log = logging.OrDiscard(cfg.Log)
	if cfg.DataDir == "" && cfg.Fabrics == nil {
		return nil, errors.New("matter: node needs somewhere to keep its fabrics")
	}
	if !cfg.Setup.Valid() {
		return nil, errors.New("matter: node needs a valid setup passcode and discriminator")
	}
	if cfg.Port == 0 {
		cfg.Port = transport.Port
	}
	if cfg.WindowTimeout == 0 {
		cfg.WindowTimeout = DefaultWindowTimeout
	}
	fabrics := cfg.Fabrics
	if fabrics == nil {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, err
		}
		fabrics = fabric.DirStore{Dir: cfg.DataDir}
	}
	n := &Node{
		cfg: cfg, log: cfg.Log, resumption: securechannel.NewResumptionStore(0),
		operational: map[uint8]*mdns.Service{}, listenMDNS: listenMDNS,
	}
	var err error
	n.salt = make([]byte, 32)
	_, _ = rand.Read(n.salt)
	if n.verifier, err = mcrypto.NewVerifier(cfg.Setup.Passcode, n.salt, pbkdfIterations); err != nil {
		return nil, err
	}
	if n.table, err = fabric.Open(fabrics); err != nil {
		return nil, err
	}
	creds := cfg.Attestation
	if creds == nil {
		if creds, err = attestation.Test(); err != nil {
			return nil, err
		}
	}
	n.comm, err = clusters.NewCommissioning(clusters.CommissioningConfig{
		Fabrics: n.table, Attestation: creds, Log: cfg.Log.With("part", "commissioning"),
		Hooks: clusters.Hooks{
			OpenWindow:    n.openWindow,
			CloseWindow:   n.closeWindow,
			FabricAdded:   n.advertiseFabric,
			FabricUpdated: n.advertiseFabric,
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
	n.server = &im.Server{
		Node: n.bridge.Node, Access: n.comm, Observer: cfg.Observer, EndSession: n.endSession,
		Log: cfg.Log.With("part", "im"),
	}
	return n, nil
}

// Bridge returns the node's bridge, to add devices to.
func (n *Node) Bridge() *clusters.Bridge { return n.bridge }

// Fabrics returns the node's fabric table.
func (n *Node) Fabrics() *fabric.Table { return n.table }

// Payload returns the node's onboarding payload, for its QR and manual pairing codes.
func (n *Node) Payload() setup.Payload {
	return PayloadFor(n.cfg.Setup)
}

// PayloadFor returns the onboarding payload of a node with setup code s.
func PayloadFor(s Setup) setup.Payload {
	return setup.Payload{
		VendorID: attestation.TestVendorID, ProductID: attestation.TestProductID,
		Flow: setup.FlowStandard, Discovery: setup.DiscoveryOnNetwork,
		Discriminator: s.Discriminator, Passcode: s.Passcode,
	}
}

// Run serves the node until ctx is done. It advertises the node on each fabric it's on, and
// opens the initial commissioning window if it's on none. After it returns it can run again, as
// after a port that was taken, and sessions start over.
func (n *Node) Run(ctx context.Context) error {
	opts := n.cfg.MDNS
	if len(n.cfg.Interfaces) > 0 {
		list, err := Interfaces(n.cfg.Interfaces)
		if err != nil {
			return err
		}
		opts.Interfaces = list
	}
	tr, err := transport.Listen(fmt.Sprintf("[::]:%d", n.cfg.Port), transport.Options{
		Log: n.log.With("part", "transport"), Panicked: n.panicked(), EndSession: n.endSession,
	})
	if err != nil {
		return err
	}
	opts.Log, opts.Panicked = n.log.With("part", "mdns"), n.panicked()
	responder, err := n.listenMDNS(opts)
	if err != nil {
		_ = tr.Serve(cancelled()) // closes the socket
		return err
	}
	defer func() { _ = responder.Close() }()

	n.server.Register(tr)
	(&securechannel.CASEResponder{
		Transport: tr, Fabrics: n.table.CASE(), Resumption: n.resumption,
		OnSession: n.sessions.add, Log: n.log.With("part", "case"),
	}).Register()
	tr.Handle(transport.SecureChannel, []uint8{securechannel.OpPBKDFParamRequest}, refusePASE)
	tr.Handle(transport.SecureChannel, []uint8{securechannel.OpStatusReport}, n.statusReport)

	// What a run before this one advertised went with its responder.
	n.mu.Lock()
	clear(n.operational)
	n.commissionable = nil
	window := n.window
	n.mu.Unlock()
	n.run.Store(&running{tr: tr, mdns: responder, ctx: ctx})

	// The socket outlives ctx by long enough to say goodbye on it.
	serving, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServing()
	served := make(chan error, 1)
	go func() { served <- tr.Serve(serving) }()
	if n.cfg.Listening != nil {
		n.cfg.Listening()
	}

	// Each fabric's controllers look for the node as soon as it's back, so advertise them all at
	// once rather than probing one after another.
	for _, f := range n.table.Fabrics() {
		go n.advertiseFabric(f.Index)
	}
	switch {
	case window != nil:
		n.openWindow(*window) // still open, on the last run's transport
	case n.table.Count() == 0:
		n.comm.OpenInitialWindow(n.cfg.WindowTimeout)
	}
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	// Closing the socket ends a write that's stuck, so a goodbye can't hold up stopping.
	defer time.AfterFunc(closeSessionsWait, stopServing).Stop()
	n.closeSessions()
	stopServing()
	return <-served
}

// closeSessions tells every controller with a session that it's over, so that it finds the node again
// now rather than at its own timeout. The mDNS goodbye comes after, as Run's responder closes.
func (n *Node) closeSessions() {
	sessions := n.sessions.list()
	for _, s := range sessions {
		n.endSession(s)
	}
	n.log.Info("Ended Matter sessions", "sessions", len(sessions))
}

// Sessions returns how many CASE sessions controllers hold on fabrics the node is still on: those a
// commissioned controller uses. A removed fabric's last a moment longer, to hear the answer.
func (n *Node) Sessions() int {
	count := 0
	for _, s := range n.sessions.list() {
		if _, ok := n.table.Fabric(s.FabricIndex()); ok && s.AuthMode() == transport.AuthCASE {
			count++
		}
	}
	return count
}

// Subscriptions returns how many subscriptions controllers hold.
func (n *Node) Subscriptions() int { return n.server.Subscriptions() }

// panicked is where the node's parts report a contained panic: the Observer, if there is one.
func (n *Node) panicked() contain.Panicked {
	if n.cfg.Observer == nil {
		return nil
	}
	return n.cfg.Observer.Panicked
}

// WindowOpen reports whether a commissioning window is open.
func (n *Node) WindowOpen() bool { return n.comm.WindowOpen() }

// openWindow starts answering PASE with w's verifier, or the node's own for a basic window,
// and advertises the commissionable service.
func (n *Node) openWindow(w clusters.Window) {
	n.mu.Lock()
	n.window = &w
	n.mu.Unlock()
	mode := mdns.CommissioningEnhanced
	if w.Basic {
		w.Verifier, w.Salt, w.Iterations, w.Discriminator = n.verifier, n.salt, pbkdfIterations, n.cfg.Setup.Discriminator
		mode = mdns.CommissioningBasic
	}
	(&securechannel.Responder{
		Transport: n.run.Load().tr, Verifier: w.Verifier, Salt: w.Salt, Iterations: int(w.Iterations),
		OnSession: n.sessions.add, Log: n.log.With("part", "pase"),
	}).Register()

	c := mdns.Commissionable{
		Instance: mdns.NewInstanceName(), Discriminator: w.Discriminator,
		VendorID: attestation.TestVendorID, ProductID: attestation.TestProductID,
		Mode: mode, DeviceType: clusters.DeviceAggregator.ID, DeviceName: n.cfg.DeviceName,
		Port: n.cfg.Port,
	}
	svc, err := c.Service()
	if err != nil {
		n.run.Load().tr.Handle(transport.SecureChannel, []uint8{securechannel.OpPBKDFParamRequest}, refusePASE)
		n.log.Error("Could not build the commissionable service", "err", err)
		return
	}
	n.mu.Lock()
	old := n.commissionable
	n.commissionable = &svc
	n.mu.Unlock()
	if old != nil {
		_ = n.run.Load().mdns.Remove(*old)
	}
	err = n.run.Load().mdns.Add(n.run.Load().ctx, svc)
	n.mu.Lock()
	current := n.commissionable == &svc
	if err != nil && current {
		n.commissionable = nil
	}
	n.mu.Unlock()
	if err != nil {
		// Without the advertisement the window isn't open: refuse PASE, so the state agrees.
		if current {
			n.run.Load().tr.Handle(transport.SecureChannel, []uint8{securechannel.OpPBKDFParamRequest}, refusePASE)
		}
		n.log.Error("Could not advertise the commissionable service", "err", err)
		return
	}
	if !current {
		// The window closed, or opened anew, while this one probed.
		_ = n.run.Load().mdns.Remove(svc)
		return
	}
	n.log.Info("Advertised commissionable service", "discriminator", w.Discriminator, "instance", svc.Instance)
}

// closeWindow stops answering PASE, and stops advertising the commissionable service.
func (n *Node) closeWindow() {
	n.run.Load().tr.Handle(transport.SecureChannel, []uint8{securechannel.OpPBKDFParamRequest}, refusePASE)
	n.mu.Lock()
	old := n.commissionable
	n.commissionable, n.window = nil, nil
	n.mu.Unlock()
	if old == nil {
		return
	}
	if err := n.run.Load().mdns.Remove(*old); err != nil {
		n.log.Error("Could not stop advertising the commissionable service", "err", err)
	}
	n.log.Info("Stopped advertising commissionable service")
	if n.table.Count() == 0 {
		n.log.Info("Closed the commissioning window while on no fabric: restart the host to open it again",
			"timeout", n.cfg.WindowTimeout)
	}
}

// refusePASE answers PASE while no commissioning window is open.
func refusePASE(ctx context.Context, ex *transport.Exchange, _ transport.Message) {
	defer ex.Close()
	_ = ex.Send(ctx, securechannel.StatusReport{
		General: securechannel.GeneralFailure, Protocol: transport.SecureChannel, Code: securechannel.CodeInvalidParameter,
	}.Message())
}

// advertiseFabric advertises the node's operational service on a fabric, which AddNOC may have
// just added, or UpdateNOC given a new node ID.
func (n *Node) advertiseFabric(index uint8) {
	f, ok := n.table.Fabric(index)
	if !ok {
		return
	}
	svc := mdns.Operational{CompressedFabricID: f.CompressedFabricID, NodeID: f.NodeID, Port: n.cfg.Port}.Service()
	n.mu.Lock()
	old := n.operational[index]
	if old != nil && old.Instance == svc.Instance {
		n.mu.Unlock()
		return
	}
	n.operational[index] = &svc
	n.mu.Unlock()
	if old != nil {
		_ = n.run.Load().mdns.Remove(*old)
	}
	err := n.run.Load().mdns.Add(n.run.Load().ctx, svc)
	n.mu.Lock()
	current := n.operational[index] == &svc
	if err != nil && current {
		delete(n.operational, index)
	}
	n.mu.Unlock()
	if err != nil {
		n.log.Error("Could not advertise the operational service", "err", err, "fabric", index)
		return
	}
	if !current {
		// The fabric was removed, or advertised anew, while this one probed.
		_ = n.run.Load().mdns.Remove(svc)
		return
	}
	n.log.Info("Advertised operational service", "fabric", index, "instance", svc.Instance)
}

// commissioned advertises a fabric CommissioningComplete committed, again for an UpdateNOC.
func (n *Node) commissioned(index uint8) {
	n.advertiseFabric(index)
	if n.cfg.FabricsChanged != nil {
		n.cfg.FabricsChanged()
	}
}

// endPASE ends every PASE session.
func (n *Node) endPASE() {
	for _, s := range n.sessions.list() {
		if s.AuthMode() == transport.AuthPASE {
			n.endSession(s)
		}
	}
}

// endSession tells the peer that s is closing, then closes it. CloseSession isn't acknowledged
// (Core spec, 4.11.1.3), so it's sent once.
func (n *Node) endSession(s *transport.Session) {
	// CloseSession means something only on a secure session: an unsecured one just goes.
	if s.Secure() {
		if ex, err := s.NewExchange(); err == nil {
			_ = ex.SendUnreliable(securechannel.StatusReport{
				General: securechannel.GeneralSuccess, Protocol: transport.SecureChannel, Code: securechannel.CodeCloseSession,
			}.Message())
			ex.Close()
		}
	}
	// Its own Transport, which may be an earlier Run's.
	s.Transport().RemoveSession(s)
}

// statusReport handles a status report a peer sends on a new exchange: CloseSession ends the
// session it came on.
func (n *Node) statusReport(_ context.Context, ex *transport.Exchange, m transport.Message) {
	defer ex.Close()
	s := ex.Session()
	r, err := securechannel.ParseStatusReport(m.Payload)
	if err != nil || !s.Secure() || r.Protocol != transport.SecureChannel || r.Code != securechannel.CodeCloseSession {
		return
	}
	n.log.Debug("Peer closed session", "session", s.LocalSessionID(), "peer", s.Peer())
	n.run.Load().tr.RemoveSession(s)
}

// fabricRemoved ends a fabric's sessions and resumptions, and its advertisement. A node left on
// no fabric opens its initial window again.
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
			n.endSession(s)
		}
	})
	n.mu.Lock()
	svc := n.operational[index]
	delete(n.operational, index)
	n.mu.Unlock()
	if svc != nil {
		if err := n.run.Load().mdns.Remove(*svc); err != nil {
			n.log.Error("Could not stop advertising the operational service", "err", err, "fabric", index)
		}
	}
	n.log.Info("Ended fabric's sessions", "fabric", index, "sessions", len(ending))
	if n.cfg.FabricsChanged != nil {
		n.cfg.FabricsChanged()
	}
	if n.table.Count() == 0 {
		n.comm.OpenInitialWindow(n.cfg.WindowTimeout)
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
