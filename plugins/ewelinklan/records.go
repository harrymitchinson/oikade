package ewelinklan

import (
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const serviceType = "_ewelink._tcp.local"

// maxServices bounds the instances followed, far beyond the eWeLink devices a home has, so that a
// host on the network inventing names can't fill memory: at the limit, a new one replaces the one
// heard from longest ago. maxAddresses bounds the addresses kept the same way.
const (
	maxServices  = 256
	maxAddresses = 2 * maxServices
)

// serviceName is a device's mDNS instance, lower-cased, and captures its ID.
var serviceName = regexp.MustCompile(`^ewelink_([0-9a-f]{10})\._ewelink\._tcp\.local$`)

// service is what's been heard about one mDNS instance, which can arrive over several packets.
type service struct {
	id     string
	txt    map[string]string
	heard  time.Time // when txt arrived
	target string
	port   uint16
}

// records follows the mDNS records of eWeLink devices.
type records struct {
	services  map[string]*service
	addresses map[string]string
}

func newRecords() *records {
	return &records{services: map[string]*service{}, addresses: map[string]string{}}
}

func clean(n dnsmessage.Name) string {
	return strings.TrimSuffix(strings.ToLower(n.String()), ".")
}

// take reads a packet, and returns the instances it brought a report from and the ones that are
// new. A packet that isn't DNS, or isn't about these devices, changes nothing.
func (r *records) take(packet []byte, now time.Time) (touched, added []string) {
	var msg dnsmessage.Message
	if err := msg.Unpack(packet); err != nil || !msg.Response {
		return nil, nil
	}
	all := append(msg.Answers, msg.Additionals...) //nolint:gocritic // msg isn't used again.
	relevant := false
	for _, rec := range all {
		relevant = relevant || isOurs(rec)
	}
	seen := map[string]bool{}
	for _, rec := range all {
		name := clean(rec.Header.Name)
		switch body := rec.Body.(type) {
		case *dnsmessage.AResource:
			// Any host's address could arrive here, so only those that may be needed are kept.
			if relevant || r.isTarget(name) {
				r.setAddress(name, netip.AddrFrom4(body.A).String())
			}
		case *dnsmessage.PTRResource:
			instance := clean(body.PTR)
			if name == serviceType && r.services[instance] == nil && r.instance(instance) != nil {
				added = append(added, instance)
			}
		case *dnsmessage.SRVResource:
			// A TTL of 0 is a goodbye, which says where the device was, not where it is.
			if s := r.instance(name); s != nil && body.Port > 0 && rec.Header.TTL > 0 {
				s.target, s.port = clean(body.Target), body.Port
			}
		case *dnsmessage.TXTResource:
			// A malformed record leaves the last good one, and a goodbye isn't a report. Only a
			// report counts as hearing from the device: an address alone doesn't.
			if fields, ok := txtFields(body.TXT); ok && rec.Header.TTL > 0 {
				if s := r.instance(name); s != nil {
					s.txt, s.heard = fields, now
					seen[name] = true
				}
			}
		}
	}
	// A name later in the packet can have replaced one at maxServices.
	for name := range seen {
		if r.services[name] != nil {
			touched = append(touched, name)
		}
	}
	added = slices.DeleteFunc(added, func(name string) bool { return r.services[name] == nil })
	return touched, added
}

func isOurs(rec dnsmessage.Resource) bool {
	name := clean(rec.Header.Name)
	return name == serviceType || serviceName.MatchString(name)
}

func (r *records) isTarget(name string) bool {
	for _, s := range r.services {
		if s.target == name {
			return true
		}
	}
	return false
}

// instance finds or adds a service. It's nil for anything that isn't one of these devices.
func (r *records) instance(name string) *service {
	m := serviceName.FindStringSubmatch(name)
	if m == nil {
		return nil
	}
	s := r.services[name]
	if s == nil {
		if len(r.services) >= maxServices {
			r.evict()
		}
		s = &service{id: m[1]}
		r.services[name] = s
	}
	return s
}

// evict forgets the service heard from longest ago, one never heard from first.
func (r *records) evict() {
	var oldest string
	for name, s := range r.services {
		if o := r.services[oldest]; o == nil || s.heard.Before(o.heard) || (s.heard.Equal(o.heard) && name < oldest) {
			oldest = name
		}
	}
	delete(r.services, oldest)
}

// setAddress keeps a host's address. At maxAddresses, it first forgets those no service points
// at, and if that frees no room, the new one is dropped.
func (r *records) setAddress(name, addr string) {
	if _, ok := r.addresses[name]; !ok && len(r.addresses) >= maxAddresses {
		targets := map[string]bool{}
		for _, s := range r.services {
			targets[s.target] = true
		}
		maps.DeleteFunc(r.addresses, func(n, _ string) bool { return !targets[n] })
		if len(r.addresses) >= maxAddresses {
			return
		}
	}
	r.addresses[name] = addr
}

// txtFields splits a TXT record into its fields. A field with no name or one repeated makes the
// whole record malformed.
func txtFields(entries []string) (map[string]string, bool) {
	fields := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return nil, false
		}
		if _, repeated := fields[name]; repeated {
			return nil, false
		}
		fields[name] = value
	}
	return fields, true
}

// address is where a service can be reached, if that's known.
func (r *records) address(s *service) (string, uint16, bool) {
	host, ok := r.addresses[s.target]
	return host, s.port, ok && s.port != 0
}

// question is one thing to ask the network.
type question struct {
	name string
	kind dnsmessage.Type
}

// query asks for every device, and for what's still needed about the ones already known.
func (r *records) query() ([]byte, error) {
	questions := []question{{serviceType, dnsmessage.TypePTR}}
	for name, s := range r.services {
		questions = append(questions, ofService(name)...)
		if s.target != "" && r.addresses[s.target] == "" {
			questions = append(questions, question{s.target, dnsmessage.TypeA})
		}
	}
	return pack(questions)
}

// ofService is what to ask about one instance.
func ofService(name string) []question {
	return []question{{name, dnsmessage.TypeTXT}, {name, dnsmessage.TypeSRV}}
}

func pack(questions []question) ([]byte, error) {
	msg := dnsmessage.Message{}
	for _, q := range questions {
		name, err := dnsmessage.NewName(q.name + ".")
		if err != nil {
			return nil, err
		}
		msg.Questions = append(msg.Questions, dnsmessage.Question{Name: name, Type: q.kind, Class: dnsmessage.ClassINET})
	}
	return msg.Pack()
}
