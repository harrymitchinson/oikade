package mdns

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// Service is a DNS-SD service instance (RFC 6763), such as a Matter node's commissionable service.
type Service struct {
	// Instance is the instance name: one DNS label, such as "B7322C948581262F".
	Instance string
	// Type is the service type and protocol, such as "_matterc._udp".
	Type string
	// Subtypes are the labels that a browser can narrow its query by, such as "_L3840". Each one
	// gets a PTR record "<subtype>._sub.<type>.local." (RFC 6763, 7.1).
	Subtypes []string
	// Port is the port the service listens on.
	Port uint16
	// Text is the TXT record's strings, each "key=value" or "key" (RFC 6763, 6).
	Text []string
}

const (
	domain = "local."
	// servicesName lists the service types on the link (RFC 6763, 9).
	servicesName = "_services._dns-sd._udp." + domain
)

var serviceType = regexp.MustCompile(`^_[A-Za-z0-9-]{1,15}\._(tcp|udp)$`)

// validLabel reports whether s can be one label of a name we build: 1 to 63 bytes, with no dots
// or backslashes, which dnsmessage would read as separators or escapes.
func validLabel(s string) bool {
	return len(s) > 0 && len(s) <= 63 && !strings.ContainsAny(s, `.\`)
}

func (s *Service) validate() error {
	switch {
	case !validLabel(s.Instance):
		return fmt.Errorf("mdns: instance name %q isn't one DNS label", s.Instance)
	case !serviceType.MatchString(s.Type):
		return fmt.Errorf("mdns: service type %q isn't _<name>._tcp or _<name>._udp", s.Type)
	case s.Port == 0:
		return errors.New("mdns: service port is 0")
	}
	for _, sub := range s.Subtypes {
		if !validLabel(sub) {
			return fmt.Errorf("mdns: subtype %q isn't one DNS label", sub)
		}
	}
	for _, t := range s.Text {
		if len(t) > 255 {
			return fmt.Errorf("mdns: TXT string longer than 255 bytes: %.20q…", t)
		}
	}
	return nil
}

// instanceName is the service instance's full name, "<instance>.<type>.local.".
func (s *Service) instanceName() (dnsmessage.Name, error) {
	return dnsmessage.NewName(s.Instance + "." + s.Type + "." + domain)
}

// records returns the service's records, without the host's addresses: the PTR records for its
// type and subtypes, the service type's entry in the services listing, and its SRV and TXT.
func (s *Service) records(host dnsmessage.Name) ([]record, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	inst, err := s.instanceName()
	if err != nil {
		return nil, fmt.Errorf("mdns: instance name: %w", err)
	}
	typ, err := dnsmessage.NewName(s.Type + "." + domain)
	if err != nil {
		return nil, fmt.Errorf("mdns: service type: %w", err)
	}
	recs := []record{
		{name: typ, typ: dnsmessage.TypePTR, ttl: otherTTL, body: &dnsmessage.PTRResource{PTR: inst}},
		{name: mustName(servicesName), typ: dnsmessage.TypePTR, ttl: otherTTL, body: &dnsmessage.PTRResource{PTR: typ}},
	}
	for _, sub := range s.Subtypes {
		n, err := dnsmessage.NewName(sub + "._sub." + s.Type + "." + domain)
		if err != nil {
			return nil, fmt.Errorf("mdns: subtype: %w", err)
		}
		recs = append(recs, record{name: n, typ: dnsmessage.TypePTR, ttl: otherTTL, body: &dnsmessage.PTRResource{PTR: inst}})
	}
	txt := s.Text
	if len(txt) == 0 {
		txt = []string{""} // an empty TXT record has one empty string (RFC 6763, 6.1)
	}
	recs = append(recs,
		record{name: inst, typ: dnsmessage.TypeSRV, unique: true, ttl: hostTTL,
			body: &dnsmessage.SRVResource{Port: s.Port, Target: host}},
		record{name: inst, typ: dnsmessage.TypeTXT, unique: true, ttl: otherTTL,
			body: &dnsmessage.TXTResource{TXT: txt}},
	)
	return recs, nil
}

func mustName(s string) dnsmessage.Name {
	n, err := dnsmessage.NewName(s)
	if err != nil {
		panic(err)
	}
	return n
}
