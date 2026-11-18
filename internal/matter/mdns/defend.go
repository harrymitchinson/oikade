package mdns

import (
	"context"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A name stays contested after it's probed: two networks join, or a host that was asleep comes
// back with the same name. A responder that hears another host answer with different data for one
// of its names must probe it again, and the loser must stop using it (RFC 6762, 9). A Responder
// stops answering for the name while it probes it again, as the other host does, so that the
// tie-break (8.2) decides between them, and then announces it again or gives it up. A service it
// gives up is reported to Options.Lost, so that the node can pick another name.
//
// The host name is defended the same way, but kept if it's lost, and logged: every service's SRV
// names it, and a Matter node names its host after its hardware address (Core spec, 4.3.4), so a
// host that takes it is misconfigured rather than unlucky.

// newProbe returns a probe for names, proposing recs and, if names holds the host name, the
// host's addresses.
func newProbe(names []dnsmessage.Name, recs []record) *probe {
	return &probe{
		names: names, records: recs,
		conflict: make(chan dnsmessage.Name, 1), lost: make(chan dnsmessage.Name, 1),
	}
}

// defenceState returns the announced services and whether the host name is announced and not
// being probed again, with the records of every service and the probes running. It answers
// packets, so its lock goes with defer.
func (r *Responder) defenceState() (announced []*entry, host bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	for _, e := range r.services {
		if e.ready {
			announced = append(announced, e)
		}
	}
	return announced, r.hostReady && !r.hostDefending
}

// defendNames starts probing again each announced name of ours that rrs hold a record of with
// other data, as when another host answers with it. ours is every record we hold, on any
// interface. A goodbye (TTL 0) claims nothing, and a shared record, a PTR, can't conflict.
func (r *Responder) defendNames(rrs []dnsmessage.Resource, ours []record) {
	announced, host := r.defenceState()
	if len(announced) == 0 && !host {
		return
	}
	conflicts := func(name dnsmessage.Name, types ...dnsmessage.Type) bool {
		for _, rr := range rrs {
			if rr.Header.TTL == 0 || !nameEqual(rr.Header.Name, name) || !slices.Contains(types, rr.Header.Type) {
				continue
			}
			if !slices.ContainsFunc(ours, func(rec record) bool { return rec.same(rr.Header.Name, rr.Header.Type, rr.Body) }) {
				return true
			}
		}
		return false
	}
	for _, e := range announced {
		if conflicts(e.name, dnsmessage.TypeSRV, dnsmessage.TypeTXT, typeNSEC) {
			r.noteConflict()
			r.after(0, func() { r.defend(e) })
		}
	}
	if host && conflicts(r.host, dnsmessage.TypeA, dnsmessage.TypeAAAA, typeNSEC) {
		r.noteConflict()
		r.after(0, r.defendHost)
	}
}

// defend probes an announced service's name again. If it keeps the name it announces the service
// again, and if another host wins, it stops advertising the service and reports it to
// Options.Lost. It says no goodbye: the other host's announcements replace our records in caches,
// and a goodbye to a record we share with it, such as the service type's PTR, would drop its too.
func (r *Responder) defend(e *entry) {
	key := strings.ToLower(e.name.String())
	r.mu.Lock()
	if r.closed || r.services[key] != e || !e.ready {
		r.mu.Unlock()
		return // gone, or already being probed again
	}
	e.ready, e.defending = false, true
	p := newProbe([]dnsmessage.Name{e.name}, e.records)
	r.probes[p] = struct{}{}
	r.mu.Unlock()
	r.log.Info("Probing mDNS name again after a conflict", "instance", e.name.String())

	err := r.probe(context.Background(), p) // Close ends it

	r.mu.Lock()
	delete(r.probes, p)
	e.defending = false
	current := !r.closed && r.services[key] == e
	switch {
	case current && err == nil:
		e.ready = true
	case current:
		delete(r.services, key)
	}
	r.mu.Unlock()
	switch {
	case !current: // removed, or closed, while it probed
	case err == nil:
		r.log.Info("Kept mDNS name after a conflict", "instance", e.name.String())
		r.announce(e)
	default:
		r.log.Warn("Lost mDNS name to another host", "instance", e.name.String(), "err", err)
		if r.lostFn != nil {
			r.lostFn(e.svc)
		}
	}
}

// defendHost probes the host name again, and announces its addresses on every interface. If
// another host wins, it logs that and keeps the name.
func (r *Responder) defendHost() {
	r.mu.Lock()
	if r.closed || !r.hostReady || r.hostDefending {
		r.mu.Unlock()
		return
	}
	r.hostDefending = true
	p := newProbe([]dnsmessage.Name{r.host}, nil)
	r.probes[p] = struct{}{}
	r.mu.Unlock()
	r.log.Info("Probing mDNS host name again after a conflict", "host", r.host.String())

	err := r.probe(context.Background(), p)

	r.mu.Lock()
	delete(r.probes, p)
	r.hostDefending = false
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	if err != nil {
		r.log.Error("Lost mDNS host name to another host, and kept it", "host", r.host.String(), "err", err)
	} else {
		r.log.Info("Kept mDNS host name after a conflict", "host", r.host.String())
	}
	for _, ifIndex := range r.served() {
		r.announceOn(ifIndex)
	}
}

// noteConflict counts a conflict, for slowConflicts.
func (r *Responder) noteConflict() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.conflicts = append(slices.DeleteFunc(r.conflicts, func(t time.Time) bool {
		return now.Sub(t) > r.timing.conflictWindow
	}), now)
}

// slowConflicts reports whether fifteen conflicts have come within ten seconds, after which each
// probe waits five seconds before it starts (RFC 6762, 8.1), so that two hosts fighting over a
// name don't flood the link.
func (r *Responder) slowConflicts() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	n := 0
	for _, t := range r.conflicts {
		if now.Sub(t) <= r.timing.conflictWindow {
			n++
		}
	}
	return n >= maxConflicts
}

// maxConflicts is how many conflicts within timing.conflictWindow slow probing (RFC 6762, 8.1).
const maxConflicts = 15
