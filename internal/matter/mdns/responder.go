// Package mdns is a Multicast DNS responder (RFC 6762) that advertises DNS-SD services (RFC 6763):
// enough for commissioners and controllers to find a Matter node, through its commissionable
// service while it's open for commissioning and an operational service per fabric (Matter Core
// Specification, 4.3). It answers queries, and resolves a service instance whose name it's given,
// as operational discovery finds a node (4.3.2). It doesn't browse.
//
// A Responder probes each service's names before it uses them, announces its records, answers
// queries for them over multicast or unicast, and says goodbye (TTL 0) to the records it stops
// advertising.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
)

// Errors from a Responder.
var (
	// ErrConflict means another host answered for a name while it was probed (RFC 6762, 9).
	ErrConflict = errors.New("mdns: name already in use on the network")
	ErrClosed   = errors.New("mdns: responder closed")
)

// Options configures a Responder.
type Options struct {
	Log *slog.Logger
	// Interfaces are the interfaces to advertise on. None means every interface that is up and
	// multicast-capable, except loopback.
	Interfaces []net.Interface
	// Host is the host name's label, without ".local". Empty means the hardware address of the
	// first interface that has one, in upper-case hex, as Matter nodes name themselves (Core
	// spec, 4.3.4), or random hex if none has.
	Host string
	// Panicked, if set, hears each panic contained in answering a packet.
	Panicked contain.Panicked
}

// timing is the protocol's delays, which tests shorten.
type timing struct {
	probeWait     time.Duration // the longest random wait before the first probe (RFC 6762, 8.1)
	probeInterval time.Duration // between probes
	announceGap   time.Duration // between the two announcements (8.3)
	minDelay      time.Duration // the random delay before a multicast answer with shared records (6)
	maxDelay      time.Duration
}

var defaultTiming = timing{
	probeWait:     250 * time.Millisecond,
	probeInterval: 250 * time.Millisecond,
	announceGap:   time.Second,
	minDelay:      20 * time.Millisecond,
	maxDelay:      120 * time.Millisecond,
}

// Responder answers mDNS queries for the services added to it.
type Responder struct {
	log      *slog.Logger
	panicked contain.Panicked
	host     dnsmessage.Name
	conns    []packetConn
	ifaces   []int
	addrs    func(ifIndex int) []netip.Addr // the host's addresses on an interface
	zones    map[int]string                 // interfaces' names, the zones of link-local addresses
	timing   timing

	// sendMu orders announcements, delayed answers and goodbyes, so that a record that has
	// been said goodbye to isn't sent again after it.
	sendMu sync.Mutex

	mu        sync.Mutex
	services  map[string]*entry // by lower-case instance name
	probes    map[*probe]struct{}
	lookups   map[*lookup]struct{} // Resolves waiting for responses
	hostReady bool                 // the host name has been probed
	closed    bool

	done chan struct{} // closed by Close, to stop timers
	wg   sync.WaitGroup
}

// entry is a service added to the Responder.
type entry struct {
	svc     Service
	name    dnsmessage.Name
	records []record // without the host's addresses
	ready   bool     // probed and announced
}

// probe is a set of names being probed. The handler reports a conflict on one of them.
type probe struct {
	names    []dnsmessage.Name
	conflict chan dnsmessage.Name // buffered, 1
}

// Listen joins the mDNS groups on the interfaces in opts, and returns a Responder that serves
// them until it's closed.
func Listen(opts Options) (*Responder, error) {
	opts.Log = logging.OrDiscard(opts.Log)
	ifaces := opts.Interfaces
	if len(ifaces) == 0 {
		var err error
		if ifaces, err = multicastInterfaces(); err != nil {
			return nil, fmt.Errorf("mdns: list interfaces: %w", err)
		}
		if len(ifaces) == 0 {
			return nil, errors.New("mdns: no multicast interface is up")
		}
	}
	var conns []packetConn
	c4, err4 := listen4(ifaces)
	if err4 == nil {
		conns = append(conns, c4)
	}
	c6, err6 := listen6(ifaces)
	if err6 == nil {
		conns = append(conns, c6)
	}
	if len(conns) == 0 {
		return nil, errors.Join(err4, err6)
	}
	if err4 != nil || err6 != nil {
		opts.Log.Info("Listening for mDNS on one address family only", "err", errors.Join(err4, err6))
	}
	host := opts.Host
	if host == "" {
		host = defaultHost(ifaces)
	}
	indexes := make([]int, len(ifaces))
	for i, ifi := range ifaces {
		indexes[i] = ifi.Index
	}
	r, err := newResponder(conns, indexes, host, interfaceAddrs, opts.Log, opts.Panicked)
	if err != nil {
		for _, c := range conns {
			_ = c.Close()
		}
		return nil, err
	}
	return r, nil
}

func newResponder(
	conns []packetConn, ifaces []int, host string, addrs func(int) []netip.Addr, log *slog.Logger, panicked contain.Panicked,
) (*Responder, error) {
	if !validLabel(host) {
		return nil, fmt.Errorf("mdns: host name %q isn't one DNS label", host)
	}
	hostName, err := dnsmessage.NewName(host + "." + domain)
	if err != nil {
		return nil, fmt.Errorf("mdns: host name: %w", err)
	}
	r := &Responder{
		log:      log,
		panicked: panicked,
		host:     hostName,
		conns:    conns,
		ifaces:   ifaces,
		addrs:    addrs,
		zones:    interfaceNames(ifaces),
		timing:   defaultTiming,
		services: map[string]*entry{},
		probes:   map[*probe]struct{}{},
		lookups:  map[*lookup]struct{}{},
		done:     make(chan struct{}),
	}
	for _, c := range conns {
		r.wg.Go(func() { r.serve(c) })
	}
	return r, nil
}

// Host returns the host name that the services' SRV records point at, such as
// "DCA632A1B2C3.local.".
func (r *Responder) Host() string { return r.host.String() }

// Add probes svc's instance name and, the first time, the host name, then announces the service
// and answers for it. It returns once the service is announced, which takes about a second, or
// with an error wrapping ErrConflict if another host already uses one of the names.
func (r *Responder) Add(ctx context.Context, svc Service) error {
	svc.Subtypes = append([]string(nil), svc.Subtypes...)
	svc.Text = append([]string(nil), svc.Text...)
	recs, err := svc.records(r.host)
	if err != nil {
		return err
	}
	name, _ := svc.instanceName() // records checked it
	key := strings.ToLower(name.String())

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	if _, ok := r.services[key]; ok {
		r.mu.Unlock()
		return fmt.Errorf("mdns: service %s already added", name)
	}
	e := &entry{svc: svc, name: name, records: recs}
	r.services[key] = e
	p := &probe{names: []dnsmessage.Name{name}, conflict: make(chan dnsmessage.Name, 1)}
	if !r.hostReady {
		p.names = append(p.names, r.host)
	}
	r.probes[p] = struct{}{}
	r.mu.Unlock()

	err = r.probe(ctx, p, e)

	r.mu.Lock()
	delete(r.probes, p)
	if err == nil && r.closed {
		err = ErrClosed
	}
	if err != nil {
		delete(r.services, key)
		r.mu.Unlock()
		return err
	}
	e.ready = true
	if len(p.names) > 1 {
		r.hostReady = true
	}
	r.mu.Unlock()

	r.log.Info("Announcing mDNS service", "instance", name.String())
	r.announce(e)
	return nil
}

// probe sends three probes for p's names, 250 ms apart, after a random wait (RFC 6762, 8.1), and
// returns an error if another host answers for one of them.
func (r *Responder) probe(ctx context.Context, p *probe, e *entry) error {
	wait := time.Duration(mrand.Int64N(int64(r.timing.probeWait) + 1))
	for i := 0; ; i++ {
		select {
		case n := <-p.conflict:
			return fmt.Errorf("%w: %s", ErrConflict, n)
		case <-ctx.Done():
			return ctx.Err()
		case <-r.done:
			return ErrClosed
		case <-time.After(wait):
		}
		if i == 3 {
			return nil
		}
		r.sendProbe(p, e)
		wait = r.timing.probeInterval
	}
}

// sendProbe asks for any record with p's names, with the records we propose for them in the
// authority section, so that a host probing at the same time can tell.
func (r *Responder) sendProbe(p *probe, e *entry) {
	r.multicast(func(ifIndex int) ([]byte, error) {
		m := dnsmessage.Message{}
		for _, n := range p.names {
			m.Questions = append(m.Questions, dnsmessage.Question{
				Name: n, Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET | unicastResponse,
			})
		}
		for _, rec := range e.records {
			if rec.unique {
				m.Authorities = append(m.Authorities, rec.resource(rec.ttl, false))
			}
		}
		if len(p.names) > 1 {
			for _, rec := range addressRecords(r.host, r.addrs(ifIndex)) {
				m.Authorities = append(m.Authorities, rec.resource(rec.ttl, false))
			}
		}
		return m.Pack()
	})
}
