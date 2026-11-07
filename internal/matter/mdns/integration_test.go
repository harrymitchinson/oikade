package mdns

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"

	"github.com/harrymitchinson/oikade/internal/logging"
)

// multicastInterface returns an IPv4 interface on which a packet sent to a multicast group comes
// back to the host, or skips the test: loopback if it can multicast, or else the first interface
// that is up and multicast-capable.
func multicastInterface(t *testing.T) net.Interface {
	t.Helper()
	all, err := net.Interfaces()
	if err != nil {
		t.Skipf("No interfaces: %v", err)
	}
	slices.SortStableFunc(all, func(a, b net.Interface) int {
		return int(b.Flags&net.FlagLoopback) - int(a.Flags&net.FlagLoopback) // loopback first
	})
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || len(interfaceAddrs(ifi.Index)) == 0 {
			continue
		}
		if multicastLoops(ifi) {
			return ifi
		}
	}
	t.Skip("No interface delivers multicast back to this host")
	return net.Interface{}
}

// multicastLoops reports whether a packet sent to an administratively scoped group on ifi comes
// back to a socket that joined it there.
func multicastLoops(ifi net.Interface) bool {
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 83, 53)}
	rc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return false
	}
	defer func() { _ = rc.Close() }()
	group.Port = rc.LocalAddr().(*net.UDPAddr).Port
	r := ipv4.NewPacketConn(rc)
	if r.JoinGroup(&ifi, group) != nil {
		return false
	}
	sc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return false
	}
	defer func() { _ = sc.Close() }()
	s := ipv4.NewPacketConn(sc)
	if s.SetMulticastInterface(&ifi) != nil || s.SetMulticastLoopback(true) != nil {
		return false
	}
	if _, err := s.WriteTo([]byte("oikade"), nil, group); err != nil {
		return false
	}
	_ = rc.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 16)
	n, _, err := rc.ReadFrom(b)
	return err == nil && string(b[:n]) == "oikade"
}

// TestMulticast advertises a commissionable service on a real interface, and queries it over
// multicast as a legacy resolver would, from a port other than 5353.
func TestMulticast(t *testing.T) {
	ifi := multicastInterface(t)
	r, err := Listen(Options{Log: logging.Test(t), Interfaces: []net.Interface{ifi}, Host: "oikade-test-" + randomHex(4)})
	if err != nil {
		t.Skipf("Could not listen for mDNS on %s: %v", ifi.Name, err)
	}
	defer func() { _ = r.Close() }()
	svc, err := Commissionable{Instance: NewInstanceName(), Discriminator: 3840, VendorID: 0xFFF1, ProductID: 0x8000, Mode: CommissioningBasic}.Service()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Add(ctx, svc); err != nil {
		t.Fatal(err)
	}

	pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	q := ipv4.NewPacketConn(pc)
	if err := q.SetMulticastInterface(&ifi); err != nil {
		t.Fatal(err)
	}
	if err := q.SetMulticastLoopback(true); err != nil {
		t.Fatal(err)
	}

	instance := svc.Instance + "._matterc._udp.local."
	ask := func(t *testing.T, name string, typ dnsmessage.Type) []dnsmessage.Resource {
		t.Helper()
		id := uint16(time.Now().UnixNano())
		if _, err := q.WriteTo(pack(t, dnsmessage.Message{
			ID:        id,
			Questions: []dnsmessage.Question{{Name: mustName(name), Type: typ, Class: dnsmessage.ClassINET}},
		}), nil, group4); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, maxPacket)
		_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				t.Fatalf("No answer to %s %v: %v", name, typ, err)
			}
			var m dnsmessage.Message
			if m.Unpack(b[:n]) == nil && m.Response && m.ID == id {
				return slices.Concat(m.Answers, m.Additionals)
			}
		}
	}
	has := func(rs []dnsmessage.Resource, name string, typ dnsmessage.Type) bool {
		return slices.ContainsFunc(rs, func(r dnsmessage.Resource) bool {
			return r.Header.Type == typ && nameEqual(r.Header.Name, mustName(name))
		})
	}

	for _, tc := range []struct {
		name string
		typ  dnsmessage.Type
		want dnsmessage.Type // among the answers, with the name asked for
	}{
		{"_matterc._udp.local.", dnsmessage.TypePTR, dnsmessage.TypePTR},
		{"_L3840._sub._matterc._udp.local.", dnsmessage.TypePTR, dnsmessage.TypePTR},
		{"_S15._sub._matterc._udp.local.", dnsmessage.TypePTR, dnsmessage.TypePTR},
		{"_V65521._sub._matterc._udp.local.", dnsmessage.TypePTR, dnsmessage.TypePTR},
		{"_CM._sub._matterc._udp.local.", dnsmessage.TypePTR, dnsmessage.TypePTR},
		{instance, dnsmessage.TypeSRV, dnsmessage.TypeSRV},
		{instance, dnsmessage.TypeTXT, dnsmessage.TypeTXT},
		{r.Host(), dnsmessage.TypeA, dnsmessage.TypeA},
	} {
		t.Run(tc.name+"/"+tc.typ.String(), func(t *testing.T) {
			rs := ask(t, tc.name, tc.typ)
			if !has(rs, tc.name, tc.want) {
				t.Errorf("no %v for %s in %v", tc.want, tc.name, rs)
			}
			if tc.typ == dnsmessage.TypePTR && tc.name != servicesName && !has(rs, instance, dnsmessage.TypeSRV) {
				t.Errorf("PTR answer without the instance's SRV: %v", rs)
			}
		})
	}
	// AAAA only where the interface has an IPv6 address.
	if slices.ContainsFunc(interfaceAddrs(ifi.Index), func(a netip.Addr) bool { return a.Is6() }) {
		if rs := ask(t, r.Host(), dnsmessage.TypeAAAA); !has(rs, r.Host(), dnsmessage.TypeAAAA) {
			t.Errorf("no AAAA for %s in %v", r.Host(), rs)
		}
	}
}
