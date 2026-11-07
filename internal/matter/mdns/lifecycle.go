package mdns

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/internal/matter/contain"
)

// Update replaces an added service's subtypes, port and TXT strings with svc's, which has the
// same instance name and type, such as when the commissioning window closes. It says goodbye to
// the records svc drops and announces the rest; it doesn't probe again, as the names are the same.
func (r *Responder) Update(svc Service) error {
	svc.Subtypes = append([]string(nil), svc.Subtypes...)
	svc.Text = append([]string(nil), svc.Text...)
	recs, err := svc.records(r.host)
	if err != nil {
		return err
	}
	name, _ := svc.instanceName()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	e, err := r.ready(name)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	old := e.records
	e.svc, e.records = svc, recs
	r.mu.Unlock()

	r.log.Info("Updating mDNS service", "instance", name.String())
	r.goodbye(without(old, recs))
	r.announce(e)
	return nil
}

// Remove says goodbye to an added service's records, and stops answering for it. svc needs only
// its instance name and type.
func (r *Responder) Remove(svc Service) error {
	name, err := svc.instanceName()
	if err != nil {
		return fmt.Errorf("mdns: instance name: %w", err)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	e, err := r.ready(name)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	delete(r.services, strings.ToLower(name.String()))
	// Keep the services listing's record while another announced service of the type is left.
	var keep []record
	for _, o := range r.services {
		if o.ready {
			keep = append(keep, o.records...)
		}
	}
	r.mu.Unlock()

	r.log.Info("Withdrawing mDNS service", "instance", name.String())
	r.goodbye(without(e.records, keep))
	return nil
}

// ready returns the added, probed service named name. r.mu is held.
func (r *Responder) ready(name dnsmessage.Name) (*entry, error) {
	e, ok := r.services[strings.ToLower(name.String())]
	switch {
	case !ok:
		return nil, fmt.Errorf("mdns: no service %s", name)
	case !e.ready:
		return nil, fmt.Errorf("mdns: service %s is still being probed", name)
	}
	return e, nil
}

// Close says goodbye to every record the Responder announced, and stops it.
func (r *Responder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	r.closed = true
	var all []record
	for _, e := range r.services {
		if e.ready {
			all = appendNew(all, e.records...)
		}
	}
	hostReady := r.hostReady
	r.mu.Unlock()

	if len(all) > 0 {
		r.goodbye(all)
	}
	if hostReady {
		r.sendMu.Lock()
		r.multicast(func(ifIndex int) ([]byte, error) {
			return response(addressRecords(r.host, r.addrs(ifIndex)), 0)
		})
		r.sendMu.Unlock()
	}
	close(r.done)
	var errs []error
	for _, c := range r.conns {
		errs = append(errs, c.Close())
	}
	r.wg.Wait()
	return errors.Join(errs...)
}

// announce sends e's records, with the host's addresses, twice, a second apart (RFC 6762, 8.3).
// Each time, it sends the records e has then, and nothing once e has been removed, so that a late
// announcement can't undo a goodbye.
func (r *Responder) announce(e *entry) {
	send := func() {
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		r.mu.Lock()
		current := !r.closed && r.services[strings.ToLower(e.name.String())] == e
		recs := e.records
		r.mu.Unlock()
		if !current {
			return
		}
		r.multicast(func(ifIndex int) ([]byte, error) {
			return response(slices.Concat(recs, addressRecords(r.host, r.addrs(ifIndex))), -1)
		})
	}
	send()
	r.after(r.timing.announceGap, send)
}

// goodbye sends recs with TTL 0, so that caches drop them (RFC 6762, 10.1). The caller has
// already stopped answering with them.
func (r *Responder) goodbye(recs []record) {
	if len(recs) == 0 {
		return
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	r.multicast(func(int) ([]byte, error) { return response(recs, 0) })
}

// after runs f after d, unless the Responder closes first.
func (r *Responder) after(d time.Duration, f func()) {
	// Close sets closed before it waits for wg, so checking it under mu keeps wg.Go from racing
	// that Wait.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.wg.Go(func() {
		defer contain.Recover(r.log, contain.MDNS, nil)
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			f()
		case <-r.done:
		}
	})
}

// multicast sends the message that build makes for each interface, on each address family.
func (r *Responder) multicast(build func(ifIndex int) ([]byte, error)) {
	for _, ifIndex := range r.ifaces {
		b, err := build(ifIndex)
		if err != nil {
			r.log.Error("Failed to build mDNS message", "err", err)
			continue
		}
		for _, c := range r.conns {
			err := c.WriteMulticast(b, ifIndex)
			switch {
			case err == nil || errors.Is(err, net.ErrClosed):
			case errors.Is(err, syscall.EADDRNOTAVAIL):
				// The interface has no address of this family yet, or ever, as with macOS's
				// utun, awdl and llw interfaces, so logging it would log every send.
			default:
				r.log.Debug("Could not send mDNS message", "interface", ifIndex, "err", err)
			}
		}
	}
}

// response packs an unsolicited multicast response with recs as answers. ttl overrides their
// TTLs unless it's negative.
func response(recs []record, ttl int64) ([]byte, error) {
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}}
	for _, rec := range recs {
		t := rec.ttl
		if ttl >= 0 {
			t = uint32(ttl)
		}
		m.Answers = append(m.Answers, rec.resource(t, true))
	}
	return m.Pack()
}

// interfaceAddrs returns the addresses of an interface.
func interfaceAddrs(ifIndex int) []netip.Addr {
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if p, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(p.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// defaultHost names the host after the first hardware address among ifaces, or at random.
func defaultHost(ifaces []net.Interface) string {
	for _, ifi := range ifaces {
		if n := len(ifi.HardwareAddr); n == 6 || n == 8 {
			return strings.ToUpper(hex.EncodeToString(ifi.HardwareAddr))
		}
	}
	return randomHex(8)
}

// randomHex returns n random bytes in upper-case hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails
	return strings.ToUpper(hex.EncodeToString(b))
}
