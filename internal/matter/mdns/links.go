package mdns

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"time"
)

// The interfaces a Responder serves, and their addresses, change while it runs: a laptop joins
// Wi-Fi, a host gets a new DHCP lease, a USB adapter is plugged in. RFC 6762 asks a responder to
// announce its records again when they change (8.4) and to say goodbye to the ones it drops (10.1).
// A Responder looks every timing.watch, as polling works the same everywhere, and joins the
// groups on an interface that comes up, announces the host's addresses again when they change,
// and says goodbye to an address that went. Until then, answers already carry the addresses an
// interface has now, so a querier that asks isn't misled, only one that cached the old ones.

// watchLinks checks the interfaces every timing.watch until the Responder closes.
func (r *Responder) watchLinks() {
	t := time.NewTicker(r.timing.watch)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.checkLinks()
		}
	}
}

// checkLinks compares the interfaces and their addresses with what the Responder last saw, joins
// the groups on an interface that's new or whose addresses changed, stops serving one that's gone,
// and tells caches.
func (r *Responder) checkLinks() {
	if r.interfaces == nil {
		return
	}
	list, err := r.interfaces()
	if err != nil {
		r.log.Debug("Could not list interfaces", "err", err)
		return
	}
	r.linkMu.Lock()
	defer r.linkMu.Unlock()
	prev := r.links
	next := make(map[int][]netip.Addr, len(list))
	names := make(map[int]string, len(list))
	var changed []int
	gone := map[int][]netip.Addr{}
	for _, ifi := range list {
		addrs := sortedAddrs(r.addrs(ifi.Index))
		next[ifi.Index], names[ifi.Index] = addrs, ifi.Name
		old, seen := prev[ifi.Index]
		unchanged := seen && slices.Equal(old, addrs)
		// Join is a no-op where a conn has joined already, so a join that failed, as on an
		// interface that has no address of the family yet on some systems, is tried each time.
		for _, c := range r.conns {
			if err := c.Join(ifi); err != nil && !unchanged {
				r.log.Debug("Could not join mDNS group", "interface", ifi.Name, "err", err)
			}
		}
		if unchanged {
			continue
		}
		if seen {
			r.log.Info("Interface's addresses changed", "interface", ifi.Name, "addrs", fmt.Sprint(addrs))
		} else {
			r.log.Info("Started advertising on interface", "interface", ifi.Name, "addrs", fmt.Sprint(addrs))
		}
		changed = append(changed, ifi.Index)
		if g := addrsWithout(old, addrs); len(g) > 0 {
			gone[ifi.Index] = g
		}
	}
	for ifIndex := range prev {
		if _, ok := next[ifIndex]; !ok {
			r.log.Info("Stopped advertising on interface", "interface", r.zoneNames()[ifIndex])
			for _, c := range r.conns {
				c.Leave(ifIndex)
			}
		}
	}

	r.links = next
	r.netMu.Lock()
	r.zones, r.ifaces = names, slices.Sorted(maps.Keys(next))
	r.netMu.Unlock()

	// Goodbyes go first, and leave out an address the host still has on another interface, as
	// when one moves between two on the same link, so that none follows the announcement of an
	// address that's still the host's.
	var held []netip.Addr
	for _, addrs := range next {
		held = append(held, addrs...)
	}
	for ifIndex, g := range gone {
		r.goodbyeAddrs(ifIndex, addrsWithout(g, held))
	}
	for _, ifIndex := range changed {
		r.announceOn(ifIndex)
	}
}

// goodbyeAddrs says goodbye to the host's addresses that an interface no longer has, once the host
// name has been announced.
func (r *Responder) goodbyeAddrs(ifIndex int, gone []netip.Addr) {
	if len(gone) == 0 {
		return
	}
	r.mu.Lock()
	announced := r.hostReady && !r.closed
	r.mu.Unlock()
	if !announced {
		return
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	r.multicastOn(ifIndex, func(int) ([]byte, error) { return response(addressRecords(r.host, gone), 0) })
}

// announceOn announces, on one interface, each announced service with the host's addresses there,
// twice, a second apart (RFC 6762, 8.3), or the addresses alone if no service is announced. Each
// time, it sends what's announced then, so a goodbye in between isn't undone.
func (r *Responder) announceOn(ifIndex int) {
	send := func() {
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		services, hostReady, open := r.announced()
		if !open || !hostReady {
			return
		}
		addrs := addressRecords(r.host, r.addrs(ifIndex))
		if len(services) == 0 {
			services = [][]record{nil}
		}
		for _, recs := range services {
			r.multicastOn(ifIndex, func(int) ([]byte, error) { return response(slices.Concat(recs, addrs), -1) })
		}
	}
	send()
	r.after(r.timing.announceGap, send)
}

// announced returns each announced service's records, whether the host name has been probed, and
// whether the Responder is still open.
func (r *Responder) announced() (services [][]record, hostReady, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false, false
	}
	keys := slices.Sorted(maps.Keys(r.services))
	for _, k := range keys {
		if e := r.services[k]; e.ready {
			services = append(services, e.records)
		}
	}
	return services, r.hostReady, true
}

// served returns the indexes of the interfaces the Responder serves.
func (r *Responder) served() []int {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	return r.ifaces
}

// serves reports whether the Responder serves an interface.
func (r *Responder) serves(ifIndex int) bool { return slices.Contains(r.served(), ifIndex) }

// zoneNames returns the names of the interfaces the Responder serves, by index. The map is never
// changed once it's returned.
func (r *Responder) zoneNames() map[int]string {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	return r.zones
}

// fixedInterfaces returns a function that lists those of ifaces that exist and are up, looked up
// by name, so that a Responder given its interfaces follows them, even one that's created again
// with a new index, but no others.
func fixedInterfaces(ifaces []net.Interface) func() ([]net.Interface, error) {
	return func() ([]net.Interface, error) {
		var out []net.Interface
		for _, want := range ifaces {
			ifi, err := net.InterfaceByName(want.Name)
			if err == nil && ifi.Flags&net.FlagUp != 0 {
				out = append(out, *ifi)
			}
		}
		return out, nil
	}
}

// sortedAddrs returns addrs unmapped and sorted, so that two lists of the same addresses compare
// equal.
func sortedAddrs(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = a.Unmap()
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

// addrsWithout returns the addresses of addrs that aren't in drop.
func addrsWithout(addrs, drop []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range addrs {
		if !slices.Contains(drop, a) {
			out = append(out, a)
		}
	}
	return out
}
