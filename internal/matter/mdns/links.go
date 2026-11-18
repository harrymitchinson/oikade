package mdns

import (
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
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
	type change struct {
		ifIndex int
		gone    []netip.Addr
	}
	var changes []change
	for _, ifi := range list {
		addrs := sortedAddrs(r.addrs(ifi.Index))
		next[ifi.Index], names[ifi.Index] = addrs, ifi.Name
		old, seen := prev[ifi.Index]
		if seen && slices.Equal(old, addrs) {
			continue
		}
		// An interface that had no address of a family when a conn opened couldn't join that
		// family's group on some systems, so try again whenever its addresses change.
		for _, c := range r.conns {
			if err := c.Join(ifi); err != nil {
				r.log.Debug("Could not join mDNS group", "interface", ifi.Name, "err", err)
			}
		}
		if seen {
			r.log.Info("Announcing interface's new addresses", "interface", ifi.Name, "addrs", joinAddrs(addrs))
		} else {
			r.log.Info("Advertising on interface", "interface", ifi.Name, "addrs", joinAddrs(addrs))
		}
		changes = append(changes, change{ifIndex: ifi.Index, gone: addrsWithout(old, addrs)})
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

	for _, ch := range changes {
		r.goodbyeAddrs(ch.ifIndex, ch.gone)
		r.announceOn(ch.ifIndex)
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

// fixedInterfaces returns a function that lists those of ifaces that still exist and are up, so
// that a Responder given its interfaces follows them but no others.
func fixedInterfaces(ifaces []net.Interface) func() ([]net.Interface, error) {
	return func() ([]net.Interface, error) {
		var out []net.Interface
		for _, want := range ifaces {
			ifi, err := net.InterfaceByIndex(want.Index)
			if err == nil && ifi.Name == want.Name && ifi.Flags&net.FlagUp != 0 {
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

func joinAddrs(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ",")
}
