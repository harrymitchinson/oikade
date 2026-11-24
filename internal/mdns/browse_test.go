package mdns

import (
	"context"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	hueService  = "_hue._tcp.local."
	hueInstance = "Hue Bridge - C84D62._hue._tcp.local."
)

func ptrRecord(service string, ttl uint32, instance string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: mustName(service), Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.PTRResource{PTR: mustName(instance)},
	}
}

// srvRecord is an instance's SRV record, on port 443 as a Hue bridge's is.
func srvRecord(instance, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: mustName(instance), Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.SRVResource{Target: mustName(target), Port: 443},
	}
}

func txtRecord(instance string, strs ...string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: mustName(instance), Class: dnsmessage.ClassINET, TTL: 4500},
		Body:   &dnsmessage.TXTResource{TXT: strs},
	}
}

func asked(q dnsmessage.Message) []string {
	out := make([]string, 0, len(q.Questions))
	for _, q := range q.Questions {
		out = append(out, q.Name.String()+" "+q.Type.String())
	}
	return out
}

func TestBrowseTakesAnInstancesRecords(t *testing.T) {
	f := newFakeAsker(t, func(_ int, q dnsmessage.Message) [][]byte {
		if got, want := asked(q), []string{hueService + " TypePTR"}; !slices.Equal(got, want) {
			t.Errorf("asked %v, want %v", got, want)
		}
		return [][]byte{
			hostResponse(t, ptrRecord("_other._tcp.local.", 120, "Printer._other._tcp.local.")),
			pack(t, dnsmessage.Message{
				Response: true,
				Answers:  []dnsmessage.Resource{ptrRecord(hueService, 120, hueInstance)},
				Additionals: []dnsmessage.Resource{
					aRecord("C42996C84D62.local.", 120, "192.0.2.20"),
					srvRecord(hueInstance, "C42996C84D62.local."),
					txtRecord(hueInstance, "bridgeid=c42996fffec84d62", "modelid=BSB003", "ModelID=other", "flag"),
					aaaaRecord("C42996C84D62.local.", 120, "fe80::20"),
					aRecord("someone-else.local.", 120, "192.0.2.99"),
				},
			}),
		}
	})
	defer func() { _ = f.Close() }()
	got, err := browse(t.Context(), f, "_hue._tcp.local", func(s []Service) bool { return len(s) > 0 })
	if err != nil {
		t.Fatal(err)
	}
	want := []Service{{
		Instance: "Hue Bridge - C84D62._hue._tcp.local", Host: "C42996C84D62.local", Port: 443,
		TXT:   map[string]string{"bridgeid": "c42996fffec84d62", "modelid": "BSB003", "flag": ""},
		Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.20")},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestBrowseTakesRecordsBeforeTheirInstance(t *testing.T) {
	f := newFakeAsker(t, func(int, dnsmessage.Message) [][]byte {
		return [][]byte{hostResponse(t,
			aRecord("C42996C84D62.local.", 120, "192.0.2.20"),
			srvRecord(hueInstance, "C42996C84D62.local."),
			txtRecord(hueInstance, "bridgeid=c42996fffec84d62"),
			ptrRecord(hueService, 120, hueInstance),
		)}
	})
	defer func() { _ = f.Close() }()
	got, err := browse(t.Context(), f, hueService, func(s []Service) bool { return len(s) > 0 })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TXT["bridgeid"] != "c42996fffec84d62" || len(got[0].Addrs) != 1 {
		t.Errorf("got %+v", got)
	}
	if n := f.queries(); n != 1 {
		t.Errorf("asked %d times, want once", n)
	}
}

func TestBrowseAsksForWhatAnAnswerLeftOut(t *testing.T) {
	shortRequery(t, 10*time.Millisecond)
	f := newFakeAsker(t, func(n int, q dnsmessage.Message) [][]byte {
		if n == 1 {
			return [][]byte{hostResponse(t, ptrRecord(hueService, 120, hueInstance))}
		}
		if got, want := asked(q), []string{hueService + " TypePTR", hueInstance + " TypeSRV", hueInstance + " TypeTXT"}; !slices.Equal(got, want) {
			t.Errorf("asked %v, want %v", got, want)
		}
		return [][]byte{hostResponse(t, srvRecord(hueInstance, "C42996C84D62.local."), txtRecord(hueInstance, "bridgeid=c42996fffec84d62"))}
	})
	defer func() { _ = f.Close() }()
	got, err := browse(t.Context(), f, hueService, func(s []Service) bool { return len(s) > 0 })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Host != "C42996C84D62.local" || got[0].TXT["bridgeid"] != "c42996fffec84d62" || got[0].Addrs != nil {
		t.Errorf("got %+v", got)
	}
}

func TestBrowseReturnsWhatItHeardWhenItsTimeIsUp(t *testing.T) {
	const second = "Hue Bridge - 0A0B0C._hue._tcp.local."
	f := newFakeAsker(t, func(n int, _ dnsmessage.Message) [][]byte {
		if n > 1 {
			return nil
		}
		return [][]byte{
			hostResponse(t, ptrRecord(hueService, 120, hueInstance), srvRecord(hueInstance, "a.local."), txtRecord(hueInstance, "bridgeid=a")),
			hostResponse(t, ptrRecord(hueService, 120, second), srvRecord(second, "b.local."), txtRecord(second, "bridgeid=b")),
			// A goodbye takes it away.
			hostResponse(t, ptrRecord(hueService, 0, hueInstance)),
		}
	})
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	got, err := browse(ctx, f, hueService, func([]Service) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TXT["bridgeid"] != "b" {
		t.Errorf("got %+v, want the second bridge alone", got)
	}
}

func TestBrowseKeepsSoManyInstances(t *testing.T) {
	b := &browsed{service: "_hue._tcp.local", instances: map[string]*Service{}, addrs: map[string][]netip.Addr{}}
	rrs := make([]dnsmessage.Resource, 0, maxBrowsed+10)
	for i := range maxBrowsed + 10 {
		rrs = append(rrs, ptrRecord(hueService, 120, string(rune('a'+i%26))+string(rune('a'+i/26))+"._hue._tcp.local."))
	}
	b.take(hostResponse(t, rrs...))
	if len(b.instances) != maxBrowsed {
		t.Errorf("kept %d instances, want %d", len(b.instances), maxBrowsed)
	}
}
