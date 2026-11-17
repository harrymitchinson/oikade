package ewelinklan

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// announcement is an mDNS response as a device sends one, with the records it names.
func announcement(id, target string, txt []string) dnsmessage.Message {
	instance := name("eWeLink_" + id + "." + serviceType)
	header := func(n dnsmessage.Name, t dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Type: t, Class: dnsmessage.ClassINET, TTL: 120}
	}
	return dnsmessage.Message{
		Response: true, Authoritative: true,
		Answers: []dnsmessage.Resource{
			{Header: header(name(serviceType), dnsmessage.TypePTR), Body: &dnsmessage.PTRResource{PTR: instance}},
			{Header: header(instance, dnsmessage.TypeSRV), Body: &dnsmessage.SRVResource{Port: 8081, Target: name(target)}},
			{Header: header(instance, dnsmessage.TypeTXT), Body: &dnsmessage.TXTResource{TXT: txt}},
		},
		Additionals: []dnsmessage.Resource{
			{Header: header(name(target), dnsmessage.TypeA), Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}}},
		},
	}
}

// FuzzRecords feeds records.take an mDNS packet, as any host on the network can send one, and
// decrypts each report it brings. Neither may panic, and what records keep stays within its
// bounds.
func FuzzRecords(f *testing.F) {
	for _, v := range vectors {
		m := announcement(relayID, "device.local", []string{"id=" + relayID, "type=th_plug", "encrypt=true", "iv=" + fixtureIV, "data1=" + v.data})
		packet, err := m.Pack()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(packet)
	}
	goodbye := announcement(probeID, "probe.local", []string{"id=" + probeID})
	goodbye.Answers[1].Header.TTL = 0
	if packet, err := goodbye.Pack(); err == nil {
		f.Add(packet)
	}
	key := lanKey(fixtureKey)
	f.Fuzz(func(t *testing.T, packet []byte) {
		r := newRecords()
		now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
		for range 2 {
			touched, _ := r.take(packet, now)
			for _, name := range touched {
				s := r.services[name]
				if s == nil {
					t.Fatalf("take touched %s, which it doesn't have", name)
				}
				if iv, data, err := ciphertext(s.txt); err == nil {
					_, _ = decrypt(iv, data, key)
				}
				_, _, _ = r.address(s)
			}
			if len(r.services) > maxServices || len(r.addresses) > maxAddresses {
				t.Fatalf("%d services and %d addresses", len(r.services), len(r.addresses))
			}
		}
		if _, err := r.query(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRecordsAreBounded: a host announcing ever new devices, each at its own host name, leaves no
// more than maxServices instances and maxAddresses addresses, and a device that reports again
// afterwards is followed again.
func TestRecordsAreBounded(t *testing.T) {
	r := newRecords()
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	for i := range 4 * maxServices {
		id := fmt.Sprintf("%010x", i)
		m := announcement(id, "host-"+id+".local", []string{"id=" + id, "type=type-" + id, "encrypt=true"})
		// Another host's address rides along, as on a busy network.
		m.Additionals = append(m.Additionals, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: name("other-" + id + ".local"), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 120},
			Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 20}},
		})
		packet, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		r.take(packet, now.Add(time.Duration(i)*time.Second))
		if len(r.services) > maxServices || len(r.addresses) > maxAddresses {
			t.Fatalf("after %d devices: %d services and %d addresses", i+1, len(r.services), len(r.addresses))
		}
	}

	m := announcement(relayID, "relay.local", []string{"id=" + relayID, "type=th_plug", "encrypt=true"})
	packet, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	touched, _ := r.take(packet, now.Add(time.Hour))
	s := r.services["ewelink_"+relayID+"."+serviceType]
	if len(touched) != 1 || s == nil {
		t.Fatalf("touched %v, service %+v", touched, s)
	}
	if host, port, ok := r.address(s); !ok || host != netip.AddrFrom4([4]byte{192, 0, 2, 10}).String() || port != 8081 {
		t.Errorf("address %s:%d, %v", host, port, ok)
	}
}

// TestDecryptRejectsWhatCBCCant: decrypt's callers check the iv and the data's length, but it
// doesn't rely on them.
func TestDecryptRejectsWhatCBCCant(t *testing.T) {
	key := lanKey(fixtureKey)
	for name, c := range map[string]struct{ iv, data []byte }{
		"no data":    {make([]byte, 16), nil},
		"not blocks": {make([]byte, 16), make([]byte, 17)},
		"short iv":   {make([]byte, 8), make([]byte, 16)},
	} {
		if _, err := decrypt(c.iv, c.data, key); !errors.Is(err, errMalformed) {
			t.Errorf("%s: %v, want errMalformed", name, err)
		}
	}
}

// TestTheBridgeIsBounded: a host announcing ever new devices of a kind it follows, and of kinds it
// doesn't, leaves it following maxServices devices and remembering maxSkipped kinds.
func TestTheBridgeIsBounded(t *testing.T) {
	env := plugin.Environment{Root: root, SecretsDir: t.TempDir()}
	b := newBridge(Config{}, env, nil, nil, fast, time.Now, slog.New(slog.DiscardHandler))
	b.lan = newFakeLAN()
	for i := range 3 * maxServices {
		id := fmt.Sprintf("%010x", i)
		kind := "th_plug"
		if i%2 == 1 {
			kind = "kind-" + id
		}
		m := announcement(id, "host-"+id+".local", []string{"id=" + id, "type=" + kind, "encrypt=true"})
		packet, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		b.take(packet)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.devices) > maxServices || len(b.skipped) > maxSkipped {
		t.Errorf("%d devices and %d skipped kinds", len(b.devices), len(b.skipped))
	}
	if !b.full {
		t.Error("the bridge never filled")
	}
}
