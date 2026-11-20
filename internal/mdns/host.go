package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// IsLocal reports whether host is a name in .local, which only mDNS answers (RFC 6762, 3).
func IsLocal(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.HasSuffix(host, ".local") && len(host) > len(".local")
}

// ErrNotFound means nothing answered for a host before the lookup ended.
var ErrNotFound = errors.New("mdns: not found")

// firstRequery is how long a lookup first waits before asking again, doubling each time up to
// maxRequery (RFC 6762, 5.2). Tests shorten it.
var firstRequery = time.Second

const maxRequery = time.Hour

// lookupTimeout bounds a .local lookup, so a host that's gone fails rather than using all of a
// dial's time. Tests shorten it.
var lookupTimeout = 5 * time.Second

// LookupHost finds the addresses of host, a name in .local, by asking on the network over mDNS:
// Go's own resolver, which a binary built without cgo uses on Linux, never does. It asks again after
// 1, 2, 4… seconds (RFC 6762, 5.2) until a response gives an address, or ctx ends, or lookupTimeout
// passes. It leaves out link-local IPv6 addresses, since it can't tell which interface they're on.
// It keeps nothing: DialContext caches what it finds.
func LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	l, err := ListenMulticast()
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Close() }()
	return lookupHost(ctx, l, host)
}

// asker is the part of a Listener a lookup uses, which tests fake.
type asker interface {
	Read(b []byte) (int, error)
	Send(b []byte) error
	Close() error
}

func lookupHost(ctx context.Context, conn asker, host string) ([]netip.Addr, error) {
	if !IsLocal(host) {
		return nil, fmt.Errorf("mdns: %s isn't in .local", host)
	}
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("mdns: host name: %w", err)
	}
	query, err := (&dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		{Name: name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET},
	}}).Pack()
	if err != nil {
		return nil, fmt.Errorf("mdns: query: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	found := make(chan []netip.Addr, 1)
	readErr := make(chan error, 1)
	go func() {
		b := make([]byte, MaxPacket)
		for {
			n, err := conn.Read(b)
			if err != nil {
				readErr <- err
				return
			}
			if addrs := addresses(b[:n], name); len(addrs) > 0 {
				found <- addrs
				return
			}
		}
	}()
	// The read loop ends once the conn closes, which the caller does after this returns.

	wait := firstRequery
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			if err := conn.Send(query); err != nil {
				return nil, fmt.Errorf("mdns: ask for %s: %w", host, err)
			}
			timer.Reset(wait)
			wait = min(2*wait, maxRequery)
		case addrs := <-found:
			return addrs, nil
		case err := <-readErr:
			return nil, fmt.Errorf("mdns: ask for %s: %w", host, err)
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: nothing on the network answered for %s over mDNS: %w", ErrNotFound, host, ctx.Err())
		}
	}
}

// addresses returns the A and AAAA records for name in a response, or nil if b isn't a response or
// has none. A record with TTL 0 is a goodbye, so it doesn't count.
func addresses(b []byte, name dnsmessage.Name) []netip.Addr {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil || !m.Response {
		return nil
	}
	var out []netip.Addr
	for _, rr := range slices.Concat(m.Answers, m.Additionals) {
		if rr.Header.TTL == 0 || !strings.EqualFold(rr.Header.Name.String(), name.String()) {
			continue
		}
		var a netip.Addr
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			a = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			a = netip.AddrFrom16(body.AAAA)
		default:
			continue
		}
		// An IPv4 link-local address needs no zone, but an IPv6 one does, and it's lost here.
		if (a.Is6() && a.IsLinkLocalUnicast()) || a.IsUnspecified() || slices.Contains(out, a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// DialContext dials address as a net.Dialer does, but finds a host in .local over mDNS first, and
// with the system's resolver only if nothing answers there, since a router's DNS may answer for
// any name. The system's resolver still reads /etc/hosts. It keeps what it finds until no address
// it found answers, and looks the host up again after a minute, dialing what it found before if
// nothing answers then. On macOS, where the system's resolver asks mDNS itself, it dials as a
// net.Dialer does.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return defaultDialer.DialContext(ctx, network, address)
}

var defaultDialer = newDialer(LookupHost, func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
})

func init() { defaultDialer.systemOnly = runtime.GOOS == "darwin" }

// lookupFunc finds a host's addresses.
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// dialer is DialContext's, with the lookups tests replace.
type dialer struct {
	mdns, system lookupFunc
	// systemOnly is for a system whose resolver does mDNS itself.
	systemOnly bool
	now        func() time.Time

	mu    sync.Mutex
	found map[string]*found // by hostKey
}

// found is a host's addresses, which a dial that finds them replaces, rather than changing them.
type found struct {
	addrs []netip.Addr
	until time.Time // when to look the host up again
}

func newDialer(mdns, system lookupFunc) *dialer {
	return &dialer{mdns: mdns, system: system, now: time.Now, found: map[string]*found{}}
}

func hostKey(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

// keepFor is how long a host's addresses are dialed before it's looked up again: less than the two
// minutes a host's own mDNS records last (RFC 6762, 10).
const keepFor = time.Minute

// dialTimeout bounds a dial whose context has no deadline, as http.DefaultTransport's does.
const dialTimeout = 30 * time.Second

// minPerAddress is the least time each address gets when the time left is split between them, as
// Go's own dialer does. Tests shorten it.
var minPerAddress = 2 * time.Second

func (d *dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	nd := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	host, port, err := net.SplitHostPort(address)
	if err != nil || !IsLocal(host) || d.systemOnly {
		return nd.DialContext(ctx, network, address)
	}
	f, stale, err := d.lookup(ctx, host)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	var candidates []netip.Addr
	for _, a := range ipv4First(f.addrs) {
		switch {
		case strings.HasSuffix(network, "4") && !a.Is4(), strings.HasSuffix(network, "6") && a.Is4():
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, &net.OpError{Op: "dial", Net: network, Err: fmt.Errorf("%s has no %s address", host, network)}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = d.now().Add(dialTimeout)
	}
	errs := make([]error, 0, len(candidates))
	for i, a := range candidates {
		// Each address gets its share of the time left, so one that never answers, such as a
		// container bridge's that a host advertises, doesn't use it all.
		share := max(deadline.Sub(d.now())/time.Duration(len(candidates)-i), minPerAddress)
		actx, cancel := context.WithTimeout(ctx, share)
		conn, err := nd.DialContext(actx, network, net.JoinHostPort(a.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	d.forget(host, f)
	if stale != nil {
		return nil, fmt.Errorf("%w; and the addresses found for it before: %w", stale, errors.Join(errs...))
	}
	return nil, errors.Join(errs...)
}

// ipv4First returns addrs unmapped, IPv4 before IPv6 and otherwise in order: on a home network,
// IPv4 is the one that's sure to route.
func ipv4First(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap())
	}
	slices.SortStableFunc(out, func(a, b netip.Addr) int {
		switch {
		case a.Is4() == b.Is4():
			return 0
		case a.Is4():
			return -1
		}
		return 1
	})
	return out
}

// lookup returns the host's addresses kept from before, or finds them afresh. A host's mDNS
// responder misses the odd query, since multicast is lossy, so if nothing finds a host it has found
// before, it returns what it found then, for another keepFor, and stale says why it couldn't find
// them afresh. Looking such a host up gets at most half the time left, so there's time to dial
// them. Only forget, once none of them answers, drops them.
func (d *dialer) lookup(ctx context.Context, host string) (f *found, stale, err error) {
	key := hostKey(host)
	d.mu.Lock()
	kept := d.found[key]
	d.mu.Unlock()
	if kept != nil && d.now().Before(kept.until) {
		return kept, nil, nil
	}
	fctx := ctx
	if kept != nil {
		left := dialTimeout
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(ctx, min(lookupTimeout, left/2))
		defer cancel()
	}
	addrs, err := d.find(fctx, host)

	d.mu.Lock()
	defer d.mu.Unlock()
	switch current := d.found[key]; {
	case err == nil:
		f = &found{addrs: addrs}
	case ctx.Err() != nil || current == nil:
		// With nobody left waiting, nothing found before, or another dial finding meanwhile that
		// none of it answers, there's nothing to dial.
		return nil, nil, err
	case current != kept:
		// Another dial found the host meanwhile, or kept what it found before.
		return current, err, nil
	default:
		f, stale = &found{addrs: kept.addrs}, err
	}
	f.until = d.now().Add(keepFor)
	d.found[key] = f
	return f, stale, nil
}

// find asks over mDNS and with the system's resolver at once, so falling back costs no more time
// than mDNS takes to give up, and takes mDNS's answer if it has one, since a router's DNS may answer
// for any name. If neither finds the host, the error says what each found.
func (d *dialer) find(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		addrs []netip.Addr
		err   error
	}
	run := func(lookup lookupFunc) chan result {
		out := make(chan result, 1)
		go func() {
			addrs, err := lookup(ctx, host)
			if err == nil && len(addrs) == 0 {
				err = fmt.Errorf("no address for %s", host)
			}
			out <- result{addrs, err}
		}()
		return out
	}
	mdns, system := run(d.mdns), run(d.system)
	m := <-mdns
	if m.err == nil {
		return m.addrs, nil
	}
	s := <-system
	if s.err != nil {
		return nil, fmt.Errorf("could not find %s: %w; and the system's resolver: %w", host, m.err, s.err)
	}
	return s.addrs, nil
}

// forget drops a host's addresses f, once none of them answers, unless another dial has found the
// host since.
func (d *dialer) forget(host string, f *found) {
	key := hostKey(host)
	d.mu.Lock()
	if d.found[key] == f {
		delete(d.found, key)
	}
	d.mu.Unlock()
}
