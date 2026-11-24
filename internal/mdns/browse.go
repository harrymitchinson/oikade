package mdns

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Service is one instance of a service on the network, as its mDNS records describe it (RFC 6763).
type Service struct {
	// Instance is its name, such as "Hue Bridge - C84D62._hue._tcp.local".
	Instance string
	// Host and Port are where it is, from its SRV record, such as "C42996C84D62.local" and 443.
	Host string
	Port uint16
	// TXT is its TXT record, with each key in lower case.
	TXT map[string]string
	// Addrs are Host's addresses, if a response gave them. DialContext finds them otherwise.
	Addrs []netip.Addr
}

// maxBrowsed bounds the instances a browse keeps, so that a host on the network inventing names
// can't fill memory.
const maxBrowsed = 64

// Browse asks for the instances of service, such as "_hue._tcp.local", and asks again after 1, 2,
// 4… seconds. It returns each instance it has both an SRV and a TXT record for once enough says
// they're all it needs, or when ctx ends, which isn't an error: set a deadline on ctx for how long
// to listen.
func Browse(ctx context.Context, service string, enough func([]Service) bool) ([]Service, error) {
	l, err := ListenMulticast()
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Close() }()
	return browse(ctx, l, service, enough)
}

func browse(ctx context.Context, conn asker, service string, enough func([]Service) bool) ([]Service, error) {
	ptr, err := dnsmessage.NewName(strings.TrimSuffix(service, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("mdns: service name: %w", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	packets := make(chan []byte, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			b := make([]byte, MaxPacket)
			n, err := conn.Read(b)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case packets <- b[:n]:
			case <-stop:
				return
			}
		}
	}()

	b := &browsed{service: hostKey(ptr.String()), instances: map[string]*Service{}, addrs: map[string][]netip.Addr{}}
	wait := firstRequery
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			query, err := b.query(ptr)
			if err != nil {
				return nil, err
			}
			if err := conn.Send(query); err != nil {
				return b.complete(), fmt.Errorf("mdns: ask for %s: %w", service, err)
			}
			timer.Reset(wait)
			wait = min(2*wait, maxRequery)
		case p := <-packets:
			if b.take(p) && enough(b.complete()) {
				return b.complete(), nil
			}
		case err := <-readErr:
			return b.complete(), fmt.Errorf("mdns: ask for %s: %w", service, err)
		case <-ctx.Done():
			return b.complete(), nil
		}
	}
}

// browsed is what a browse has heard, by lower-cased name.
type browsed struct {
	service   string
	instances map[string]*Service
	addrs     map[string][]netip.Addr
}

// query asks for the service's instances, and for the SRV and TXT records of any instance heard
// without them, which a responder may leave out of its answer.
func (b *browsed) query(ptr dnsmessage.Name) ([]byte, error) {
	questions := []dnsmessage.Question{{Name: ptr, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}}
	for _, key := range slices.Sorted(maps.Keys(b.instances)) {
		s := b.instances[key]
		name, err := dnsmessage.NewName(s.Instance + ".")
		if err != nil {
			continue
		}
		if s.Host == "" {
			questions = append(questions, dnsmessage.Question{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET})
		}
		if s.TXT == nil {
			questions = append(questions, dnsmessage.Question{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET})
		}
	}
	query, err := (&dnsmessage.Message{Questions: questions}).Pack()
	if err != nil {
		return nil, fmt.Errorf("mdns: query: %w", err)
	}
	return query, nil
}

// take reads a packet, and reports whether it changed anything. A record with TTL 0 is a goodbye.
func (b *browsed) take(packet []byte) bool {
	var m dnsmessage.Message
	if err := m.Unpack(packet); err != nil || !m.Response {
		return false
	}
	records := slices.Concat(m.Answers, m.Additionals)
	changed := false
	// Instances, then their records, then addresses, since a packet may give them in any order.
	for _, rr := range records {
		body, ok := rr.Body.(*dnsmessage.PTRResource)
		if !ok || hostKey(rr.Header.Name.String()) != b.service {
			continue
		}
		instance := strings.TrimSuffix(body.PTR.String(), ".")
		key := hostKey(instance)
		switch {
		case rr.Header.TTL == 0:
			changed = changed || b.instances[key] != nil
			delete(b.instances, key)
		case b.instances[key] == nil && len(b.instances) < maxBrowsed:
			b.instances[key] = &Service{Instance: instance}
			changed = true
		}
	}
	for _, rr := range records {
		name := hostKey(rr.Header.Name.String())
		switch body := rr.Body.(type) {
		case *dnsmessage.SRVResource:
			if s := b.instances[name]; s != nil && rr.Header.TTL > 0 {
				s.Host, s.Port = strings.TrimSuffix(body.Target.String(), "."), body.Port
				changed = true
			}
		case *dnsmessage.TXTResource:
			if s := b.instances[name]; s != nil && rr.Header.TTL > 0 {
				s.TXT = txt(body.TXT)
				changed = true
			}
		}
	}
	for _, rr := range records {
		name := hostKey(rr.Header.Name.String())
		if rr.Header.TTL == 0 || !b.isHost(name) {
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
		// An IPv6 link-local address needs its interface, which is lost here.
		if (a.Is6() && a.IsLinkLocalUnicast()) || a.IsUnspecified() || slices.Contains(b.addrs[name], a) {
			continue
		}
		b.addrs[name] = append(b.addrs[name], a)
		changed = true
	}
	return changed
}

func (b *browsed) isHost(name string) bool {
	for _, s := range b.instances {
		if hostKey(s.Host) == name {
			return true
		}
	}
	return false
}

// complete is every instance with both its SRV and TXT records, sorted by name.
func (b *browsed) complete() []Service {
	var out []Service
	for _, s := range b.instances {
		if s.Host == "" || s.TXT == nil {
			continue
		}
		c := *s
		c.TXT = maps.Clone(s.TXT)
		c.Addrs = slices.Clone(b.addrs[hostKey(s.Host)])
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y Service) int { return cmp.Compare(hostKey(x.Instance), hostKey(y.Instance)) })
	return out
}

// txt reads a TXT record's strings, each key=value or a bare key (RFC 6763, 6.3). The first of a
// key repeated counts.
func txt(strs []string) map[string]string {
	out := make(map[string]string, len(strs))
	for _, s := range strs {
		key, value, _ := strings.Cut(s, "=")
		key = strings.ToLower(key)
		if _, ok := out[key]; key != "" && !ok {
			out[key] = value
		}
	}
	return out
}
