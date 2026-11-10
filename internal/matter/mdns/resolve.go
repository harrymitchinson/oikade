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

// ErrNotFound means no host answered for a service instance before the lookup's context ended.
var ErrNotFound = errors.New("mdns: not found")

// firstRequery is how long a lookup first waits before asking again. It doubles each time, as RFC 6762,
// 5.2 has for a continuous query, and tests shorten it.
var firstRequery = time.Second

// Resolved is where a service instance is: its SRV record's target and port, and the target's
// addresses.
type Resolved struct {
	Instance string // the instance's full name, such as "2906C908D115D362-8FC7772401CD0696._matter._tcp.local."
	Host     string // the SRV target, such as "DCA632A1B2C3.local."
	Port     uint16
	// Addrs are the host's addresses with the port, each with the zone of the interface it was
	// heard on if it's link-local.
	Addrs []netip.AddrPort
}

// lookup is a ResolveOperational waiting for the records it needs. The read loop hands it every response.
type lookup struct {
	instance dnsmessage.Name
	resp     chan heard // buffered
}

// heard is a response and the interface it came in on.
type heard struct {
	msg     *dnsmessage.Message
	ifIndex int
}

// ResolveOperational finds a commissioned node on a fabric by operational discovery (Core spec,
// 4.3.2): the instance "<compressed fabric ID>-<node ID>" of _matter._tcp. It asks for the
// instance's SRV record, then for the addresses of the SRV's target if no answer carried them,
// asking again after 1, 2, 4… seconds, up to an hour (RFC 6762, 5.2), until it has both or ctx
// ends. It's one-shot: it doesn't follow changes. It returns an error wrapping ErrNotFound if
// nothing answers before ctx ends.
func (r *Responder) ResolveOperational(ctx context.Context, compressedFabricID, nodeID uint64) (Resolved, error) {
	// The instance is hex digits and a hyphen, so it needs no escaping (RFC 6763, 4.3).
	instance := Operational{CompressedFabricID: compressedFabricID, NodeID: nodeID}.Service().Instance
	name, err := dnsmessage.NewName(instance + "." + OperationalType + "." + domain)
	if err != nil {
		return Resolved{}, fmt.Errorf("mdns: instance name: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotFound, name, err)
	}
	l := &lookup{instance: name, resp: make(chan heard, 16)}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Resolved{}, ErrClosed
	}
	r.lookups[l] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.lookups, l)
		r.mu.Unlock()
	}()

	st := resolving{instance: name, addrs: map[string][]netip.Addr{}}
	r.query(name, st.target)
	wait := firstRequery
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case h := <-l.resp:
			hadTarget := st.target.Length > 0
			st.take(h, r.zones)
			if found, ok := st.resolved(); ok {
				r.log.Debug("Resolved mDNS service", "instance", found.Instance, "host", found.Host, "addrs", fmt.Sprint(found.Addrs))
				return found, nil
			}
			if !hadTarget && st.target.Length > 0 {
				// Ask for the addresses now, rather than at the next requery, which may be a while.
				r.query(name, st.target)
				wait = firstRequery
				timer.Reset(wait)
			}
		case <-timer.C:
			r.query(name, st.target)
			wait = min(2*wait, maxRequery)
			timer.Reset(wait)
		case <-ctx.Done():
			return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotFound, name, ctx.Err())
		case <-r.done:
			return Resolved{}, ErrClosed
		}
	}
}

// maxRequery is the longest a lookup waits before asking again (RFC 6762, 5.2).
const maxRequery = time.Hour

// resolving is what a lookup has heard: the instance's SRV target and port, if any, and every
// address it has heard for any name, since a response may carry a host's addresses before the
// SRV that names it.
type resolving struct {
	instance dnsmessage.Name
	target   dnsmessage.Name
	port     uint16
	addrs    map[string][]netip.Addr // by lower-case name
}

// take applies a response's records. A record with TTL 0 is a goodbye, and withdraws what it
// names. A link-local address is kept with the zone of the interface it was heard on, and left
// out if that interface has no name.
func (st *resolving) take(h heard, zones map[int]string) {
	for _, rr := range slices.Concat(h.msg.Answers, h.msg.Additionals) {
		key := strings.ToLower(rr.Header.Name.String())
		gone := rr.Header.TTL == 0
		var a netip.Addr
		switch body := rr.Body.(type) {
		case *dnsmessage.SRVResource:
			if !nameEqual(rr.Header.Name, st.instance) {
				continue
			}
			switch {
			case !gone:
				st.target, st.port = body.Target, body.Port
			case nameEqual(st.target, body.Target):
				st.target, st.port = dnsmessage.Name{}, 0
			}
			continue
		case *dnsmessage.AResource:
			a = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			a = netip.AddrFrom16(body.AAAA)
			if a.IsLinkLocalUnicast() {
				zone := zones[h.ifIndex]
				if zone == "" {
					continue
				}
				a = a.WithZone(zone)
			}
		default:
			continue
		}
		have := st.addrs[key]
		switch i := slices.Index(have, a); {
		case gone && i >= 0:
			st.addrs[key] = slices.Delete(have, i, i+1)
		case !gone && i < 0:
			st.addrs[key] = append(have, a)
		}
	}
}

// resolved returns where the instance is, once its target and an address of it are known.
func (st *resolving) resolved() (Resolved, bool) {
	addrs := st.addrs[strings.ToLower(st.target.String())]
	if st.target.Length == 0 || len(addrs) == 0 {
		return Resolved{}, false
	}
	found := Resolved{Instance: st.instance.String(), Host: st.target.String(), Port: st.port}
	for _, a := range addrs {
		found.Addrs = append(found.Addrs, netip.AddrPortFrom(a, st.port))
	}
	return found, true
}

// query multicasts the questions a lookup still needs answered: the instance's SRV until it knows
// the target, then the target's A and AAAA. The answers come back to every querier, over
// multicast, so they also reach a lookup whose socket shares the port with another responder.
func (r *Responder) query(instance, target dnsmessage.Name) {
	qs := []dnsmessage.Question{{Name: instance, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET}}
	if target.Length > 0 {
		qs = []dnsmessage.Question{
			{Name: target, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
			{Name: target, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET},
		}
	}
	b, err := (&dnsmessage.Message{Questions: qs}).Pack()
	if err != nil {
		r.log.Error("Failed to build mDNS query", "err", err)
		return
	}
	r.multicast(func(int) ([]byte, error) { return b, nil })
}

// deliver hands a response to the lookups waiting, without blocking the read loop: a lookup that
// is behind misses it, and asks again.
func (r *Responder) deliver(m *dnsmessage.Message, ifIndex int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for l := range r.lookups {
		select {
		case l.resp <- heard{msg: m, ifIndex: ifIndex}:
		default:
		}
	}
}

// interfaceNames returns the names of the interfaces with the given indexes, the zones of
// link-local addresses heard on them.
func interfaceNames(ifaces []int) map[int]string {
	names := map[int]string{}
	for _, i := range ifaces {
		if ifi, err := net.InterfaceByIndex(i); err == nil {
			names[i] = ifi.Name
		}
	}
	return names
}
