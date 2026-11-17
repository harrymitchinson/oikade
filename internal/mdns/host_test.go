package mdns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestIsLocal(t *testing.T) {
	for host, want := range map[string]bool{
		"gateway-1234-5678-9012.local": true,
		"Europa.LOCAL.":                true,
		"broker.local":                 true,
		"local":                        false,
		".local":                       false,
		"broker.localdomain":           false,
		"example.com":                  false,
		"192.168.1.40":                 false,
	} {
		if got := IsLocal(host); got != want {
			t.Errorf("IsLocal(%q) = %v, want %v", host, got, want)
		}
	}
}

// fakeAsker is an asker whose network is answer: it's given each query sent, and what it returns
// is read back, in order.
type fakeAsker struct {
	t      *testing.T
	answer func(n int, query dnsmessage.Message) [][]byte
	in     chan []byte
	closed chan struct{}
	once   sync.Once

	mu   sync.Mutex
	sent []dnsmessage.Message
}

func newFakeAsker(t *testing.T, answer func(n int, query dnsmessage.Message) [][]byte) *fakeAsker {
	t.Helper()
	return &fakeAsker{t: t, answer: answer, in: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakeAsker) Read(b []byte) (int, error) {
	select {
	case p := <-f.in:
		return copy(b, p), nil
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeAsker) Send(b []byte) error {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		f.t.Errorf("sent a query that doesn't parse: %v", err)
	}
	f.mu.Lock()
	f.sent = append(f.sent, m)
	n := len(f.sent)
	f.mu.Unlock()
	f.in <- b // the Listener hears its own query, since multicast loops back
	for _, p := range f.answer(n, m) {
		f.in <- p
	}
	return nil
}

func (f *fakeAsker) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeAsker) queries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func shortRequery(t *testing.T, d time.Duration) {
	t.Helper()
	old := firstRequery
	firstRequery = d
	t.Cleanup(func() { firstRequery = old })
}

func pack(t testing.TB, m dnsmessage.Message) []byte {
	t.Helper()
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustName(s string) dnsmessage.Name { return dnsmessage.MustNewName(s) }

func hostResponse(t *testing.T, rrs ...dnsmessage.Resource) []byte {
	t.Helper()
	return pack(t, dnsmessage.Message{Response: true, Authoritative: true, Answers: rrs})
}

func aRecord(name string, ttl uint32, a string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: mustName(name), Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.AResource{A: netip.MustParseAddr(a).As4()},
	}
}

func aaaaRecord(name string, ttl uint32, a string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: mustName(name), Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr(a).As16()},
	}
}

func TestLookupHostTakesTheHostsAddresses(t *testing.T) {
	f := newFakeAsker(t, func(_ int, q dnsmessage.Message) [][]byte {
		asked := make([]string, 0, len(q.Questions))
		for _, q := range q.Questions {
			asked = append(asked, q.Name.String()+" "+q.Type.String())
		}
		if want := []string{"hub.local. TypeA", "hub.local. TypeAAAA"}; !slices.Equal(asked, want) {
			t.Errorf("asked %v, want %v", asked, want)
		}
		return [][]byte{
			// Someone else's answer, and a response with only a goodbye, count for nothing.
			hostResponse(t, aRecord("other.local.", 120, "192.0.2.9")),
			hostResponse(t, aRecord("hub.local.", 0, "192.0.2.8")),
			hostResponse(t,
				aRecord("HUB.local.", 120, "192.0.2.7"),
				aaaaRecord("hub.local.", 120, "fe80::7"), // which interface it's on is lost
				aaaaRecord("hub.local.", 120, "2001:db8::7"),
				aRecord("hub.local.", 120, "192.0.2.7"),
			),
		}
	})
	defer func() { _ = f.Close() }()
	got, err := lookupHost(t.Context(), f, "hub.local")
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("2001:db8::7")}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLookupHostAsksAgain(t *testing.T) {
	shortRequery(t, 10*time.Millisecond)
	f := newFakeAsker(t, func(n int, _ dnsmessage.Message) [][]byte {
		if n < 3 {
			return nil
		}
		return [][]byte{hostResponse(t, aRecord("hub.local.", 120, "192.0.2.7"))}
	})
	defer func() { _ = f.Close() }()
	got, err := lookupHost(t.Context(), f, "hub.local.")
	if err != nil {
		t.Fatal(err)
	}
	if want := []netip.Addr{netip.MustParseAddr("192.0.2.7")}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if n := f.queries(); n != 3 {
		t.Errorf("asked %d times, want 3", n)
	}
}

func TestLookupHostGivesUp(t *testing.T) {
	old := lookupTimeout
	lookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = old })
	f := newFakeAsker(t, func(int, dnsmessage.Message) [][]byte { return nil })
	defer func() { _ = f.Close() }()
	_, err := lookupHost(t.Context(), f, "gone.local")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "gone.local") {
		t.Errorf("got %v, want ErrNotFound naming gone.local", err)
	}
}

func TestLookupHostRefusesOtherNames(t *testing.T) {
	f := newFakeAsker(t, func(int, dnsmessage.Message) [][]byte { return nil })
	defer func() { _ = f.Close() }()
	if _, err := lookupHost(t.Context(), f, "example.com"); err == nil {
		t.Error("looked up example.com over mDNS")
	}
	if n := f.queries(); n != 0 {
		t.Errorf("asked %d times, want 0", n)
	}
}

// listenLoopback is a TCP listener on address that accepts and closes, and its port. It skips t
// if address can't be listened on, as ::1 can't where there's no IPv6.
func listenLoopback(t *testing.T, address string) string {
	t.Helper()
	l, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("can't listen on %s: %v", address, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func found(addrs ...string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func failed(msg string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New(msg) }
}

// waitsForCtx is a lookup that never answers.
func waitsForCtx(ctx context.Context, _ string) ([]netip.Addr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDialerDialsWhicheverLookupFindsTheHost(t *testing.T) {
	port := listenLoopback(t, "127.0.0.1:0")
	for name, d := range map[string]dialer{
		"mdns":   {mdns: found("127.0.0.1"), system: failed("no such host")},
		"system": {mdns: waitsForCtx, system: found("::ffff:127.0.0.1")},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("hub.local", port))
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		})
	}
}

func TestDialerTriesTheNextAddress(t *testing.T) {
	// Only ::1 listens, so 127.0.0.1, which it tries first, refuses.
	port := listenLoopback(t, "[::1]:0")
	d := dialer{mdns: found("::1", "127.0.0.1"), system: waitsForCtx}
	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("hub.local", port))
	if err != nil {
		t.Fatal(err)
	}
	if got := conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != "::1" {
		t.Errorf("reached %s, want ::1", got)
	}
	_ = conn.Close()
}

func TestDialerDialsOnlyTheNetworksAddresses(t *testing.T) {
	d := dialer{mdns: found("127.0.0.1"), system: waitsForCtx}
	_, err := d.DialContext(t.Context(), "tcp6", "hub.local:8443")
	if err == nil || !strings.Contains(err.Error(), "hub.local has no tcp6 address") {
		t.Errorf("got %v, want hub.local has no tcp6 address", err)
	}
}

func TestIPv4First(t *testing.T) {
	in := []netip.Addr{
		netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("::ffff:192.0.2.1"),
		netip.MustParseAddr("2001:db8::2"), netip.MustParseAddr("192.0.2.2"),
	}
	got := make([]string, 0, len(in))
	for _, a := range ipv4First(in) {
		got = append(got, a.String())
	}
	if want := []string{"192.0.2.1", "192.0.2.2", "2001:db8::1", "2001:db8::2"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDialerSaysWhatEachLookupFound(t *testing.T) {
	d := dialer{mdns: failed("nothing answered over mDNS"), system: failed("no such host")}
	_, err := d.DialContext(t.Context(), "tcp", "hub.local:8443")
	if err == nil {
		t.Fatal("dialed a host nobody found")
	}
	for _, want := range []string{"hub.local", "nothing answered over mDNS", "no such host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q doesn't say %q", err, want)
		}
	}
}

func TestDialerDialsOtherHostsDirectly(t *testing.T) {
	port := listenLoopback(t, "127.0.0.1:0")
	d := dialer{mdns: failed("asked mDNS"), system: failed("asked the resolver")}
	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}
