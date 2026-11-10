package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// ErrNotFound means no host answered for a service instance before the lookup's context ended.
var ErrNotFound = errors.New("mdns: not found")

// firstRequery is how long a lookup waits before asking again. It doubles each time, as RFC 6762,
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

// lookup is a Resolve waiting for the records it needs. The read loop hands it every response.
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
// 4.3.2): the instance "<compressed fabric ID>-<node ID>" of _matter._tcp. It returns an error
// wrapping ErrNotFound if nothing answers before ctx ends.
func (r *Responder) ResolveOperational(ctx context.Context, compressedFabricID, nodeID uint64) (Resolved, error) {
	instance := Operational{CompressedFabricID: compressedFabricID, NodeID: nodeID}.Service().Instance
	return r.Resolve(ctx, instance, OperationalType)
}

// Resolve finds a service instance: it asks for its SRV record, then for the addresses of the
// SRV's target if no answer carried them, asking again after 1, 2, 4… seconds, until it has
// both or ctx ends. It's one-shot: it doesn't follow changes.
func (r *Responder) Resolve(ctx context.Context, instance, serviceType string) (Resolved, error) {
	name, err := dnsmessage.NewName(instance + "." + serviceType + "." + domain)
	if err != nil {
		return Resolved{}, fmt.Errorf("mdns: instance name: %w", err)
	}
	l := &lookup{instance: name, resp: make(chan heard, 16)}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Resolved{}, ErrClosed
	}
	if r.lookups == nil {
		r.lookups = map[*lookup]struct{}{}
	}
	r.lookups[l] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.lookups, l)
		r.mu.Unlock()
	}()

	var target dnsmessage.Name
	var port uint16
	var addrs []netip.AddrPort
	found := Resolved{Instance: name.String()}
	r.query(name, target)
	wait := firstRequery
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case h := <-l.resp:
			target, port, addrs = r.take(h, name, target, port, addrs)
			if target.Length > 0 && len(addrs) > 0 {
				found.Host, found.Port, found.Addrs = target.String(), port, addrs
				r.log.Debug("Resolved mDNS service", "instance", found.Instance, "host", found.Host, "addrs", fmt.Sprint(found.Addrs))
				return found, nil
			}
		case <-timer.C:
			r.query(name, target)
			wait *= 2
			timer.Reset(wait)
		case <-ctx.Done():
			return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotFound, name, ctx.Err())
		case <-r.done:
			return Resolved{}, ErrClosed
		}
	}
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

// take adds what a response says about the instance to what a lookup knows: the SRV's target and
// port, and the target's addresses. A record with TTL 0 is a goodbye, and is ignored.
func (r *Responder) take(h heard, instance, target dnsmessage.Name, port uint16, addrs []netip.AddrPort) (dnsmessage.Name, uint16, []netip.AddrPort) {
	rrs := slices.Concat(h.msg.Answers, h.msg.Additionals)
	if target.Length == 0 {
		for _, rr := range rrs {
			if srv, ok := rr.Body.(*dnsmessage.SRVResource); ok && rr.Header.TTL > 0 && nameEqual(rr.Header.Name, instance) {
				target, port = srv.Target, srv.Port
				break
			}
		}
		if target.Length == 0 {
			return target, port, addrs
		}
	}
	for _, rr := range rrs {
		if rr.Header.TTL == 0 || !nameEqual(rr.Header.Name, target) {
			continue
		}
		var a netip.Addr
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			a = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			a = netip.AddrFrom16(body.AAAA)
			if a.IsLinkLocalUnicast() {
				a = a.WithZone(r.zone(h.ifIndex))
			}
		default:
			continue
		}
		if ap := netip.AddrPortFrom(a, port); !slices.Contains(addrs, ap) {
			addrs = append(addrs, ap)
		}
	}
	return target, port, addrs
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

// interfaceName is the zone of a link-local address heard on an interface.
func interfaceName(ifIndex int) string {
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return ""
	}
	return ifi.Name
}
