package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
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

// lookupTimeout bounds a .local lookup whose context has no deadline, so a host that's gone fails
// rather than waiting for ever. Tests shorten it.
var lookupTimeout = 5 * time.Second

// LookupHost finds the addresses of host, a name in .local, by asking on the network over mDNS:
// Go's own resolver, which a binary built without cgo uses on Linux, never does. It asks again after
// 1, 2, 4… seconds (RFC 6762, 5.2) until a response gives an address, or ctx ends, or lookupTimeout
// passes. It leaves out link-local IPv6 addresses, since it can't tell which interface they're on.
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
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
	}

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
		if a.IsLinkLocalUnicast() || a.IsUnspecified() || slices.Contains(out, a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// DialContext dials address as a net.Dialer does, but finds a host in .local over mDNS as well as
// with the system's resolver, and dials whichever finds it first. The system's resolver still
// answers from /etc/hosts, and on macOS asks mDNS itself.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return defaultDialer.DialContext(ctx, network, address)
}

var defaultDialer = dialer{
	mdns: LookupHost,
	system: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	},
}

// dialer is DialContext's, with the lookups tests replace.
type dialer struct {
	mdns, system func(ctx context.Context, host string) ([]netip.Addr, error)
}

func (d dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var nd net.Dialer
	host, port, err := net.SplitHostPort(address)
	if err != nil || !IsLocal(host) {
		return nd.DialContext(ctx, network, address)
	}
	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	var errs []error
	for _, a := range ipv4First(addrs) {
		switch {
		case strings.HasSuffix(network, "4") && !a.Is4(), strings.HasSuffix(network, "6") && a.Is4():
			continue
		}
		conn, err := nd.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 {
		return nil, &net.OpError{Op: "dial", Net: network, Err: fmt.Errorf("%s has no %s address", host, network)}
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

// resolve runs both lookups at once, and returns the first that finds an address. If neither does,
// the error says what each found.
func (d dialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		addrs []netip.Addr
		err   error
	}
	run := func(lookup func(context.Context, string) ([]netip.Addr, error)) chan result {
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
	var mdnsErr, systemErr error
	for mdns != nil || system != nil {
		select {
		case r := <-mdns:
			if r.err == nil {
				return r.addrs, nil
			}
			mdnsErr, mdns = r.err, nil
		case r := <-system:
			if r.err == nil {
				return r.addrs, nil
			}
			systemErr, system = r.err, nil
		}
	}
	return nil, fmt.Errorf("could not find %s: %w; and the system's resolver: %w", host, mdnsErr, systemErr)
}
