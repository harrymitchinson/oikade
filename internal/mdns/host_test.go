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
				aRecord("hub.local.", 120, "169.254.0.7"),
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
	want := []netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("169.254.0.7"), netip.MustParseAddr("2001:db8::7")}
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

func answers(addrs ...string) lookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func failed(msg string) lookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New(msg) }
}

func TestDialerDialsWhatALookupFinds(t *testing.T) {
	port := listenLoopback(t, "127.0.0.1:0")
	for name, d := range map[string]*dialer{
		"mdns":   newDialer(answers("127.0.0.1"), failed("no such host")),
		"system": newDialer(failed("nothing answered over mDNS"), answers("::ffff:127.0.0.1")),
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

// A router's DNS may answer for any name, and sooner than mDNS does, so mDNS's answer wins.
func TestDialerPrefersMDNS(t *testing.T) {
	port := listenLoopback(t, "127.0.0.1:0")
	slow := func(ctx context.Context, host string) ([]netip.Addr, error) {
		time.Sleep(50 * time.Millisecond)
		return answers("127.0.0.1")(ctx, host)
	}
	// 192.0.2.1 is TEST-NET-1, which nothing answers.
	d := newDialer(slow, answers("192.0.2.1"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("hub.local", port))
	if err != nil {
		t.Fatal(err)
	}
	if got := conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
		t.Errorf("reached %s, want 127.0.0.1", got)
	}
	_ = conn.Close()
}

func TestDialerKeepsWhatItFound(t *testing.T) {
	port := listenLoopback(t, "127.0.0.1:0")
	var mu sync.Mutex
	asked := 0
	addr := "127.0.0.1"
	d := newDialer(func(ctx context.Context, host string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		return answers(addr)(ctx, host)
	}, failed("no such host"))
	now := time.Now()
	d.now = func() time.Time { return now }
	dial := func(address string) error {
		conn, err := d.DialContext(t.Context(), "tcp", address)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	expect := func(want int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if asked != want {
			t.Errorf("asked %d times, want %d", asked, want)
		}
	}
	if err := dial(net.JoinHostPort("hub.local", port)); err != nil {
		t.Fatal(err)
	}
	if err := dial(net.JoinHostPort("HUB.local.", port)); err != nil {
		t.Fatal(err)
	}
	expect(1)
	now = now.Add(keepFor)
	if err := dial(net.JoinHostPort("hub.local", port)); err != nil {
		t.Fatal(err)
	}
	expect(2)
	// Once nothing it kept answers, it asks again.
	if err := dial("hub.local:1"); err == nil {
		t.Fatal("dialed port 1")
	}
	if err := dial(net.JoinHostPort("hub.local", port)); err != nil {
		t.Fatal(err)
	}
	expect(3)
}

// misses is an mDNS lookup that finds 127.0.0.1 until it's told to miss, as a host's responder
// misses the odd query, and counts how often it's asked.
type misses struct {
	mu     sync.Mutex
	asked  int
	missed bool
}

func (m *misses) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked++
	if m.missed {
		return nil, errors.New("nothing answered over mDNS")
	}
	return answers("127.0.0.1")(ctx, host)
}

func (m *misses) miss() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.missed = true
}

func (m *misses) expect(t *testing.T, asked int) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.asked != asked {
		t.Errorf("asked %d times, want %d", m.asked, asked)
	}
}

// missingDialer is a dialer whose mDNS lookups are m's, whose system resolver finds nothing, and
// whose clock moves only when the test moves it.
func missingDialer(m *misses) (*dialer, *time.Time) {
	d := newDialer(m.lookup, failed("no such host"))
	now := time.Now()
	d.now = func() time.Time { return now }
	return d, &now
}

func dialOnce(ctx context.Context, d *dialer, address string) error {
	conn, err := d.DialContext(ctx, "tcp", address)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func TestDialerDialsWhatItFoundBeforeWhenALookupMisses(t *testing.T) {
	address := net.JoinHostPort("hub.local", listenLoopback(t, "127.0.0.1:0"))
	m := &misses{}
	d, now := missingDialer(m)
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatal(err)
	}
	m.miss()
	*now = now.Add(keepFor)
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatalf("a lookup that missed: %v", err)
	}
	m.expect(t, 2)
	// It keeps them for another minute, rather than waiting on a lookup at every dial.
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatal(err)
	}
	m.expect(t, 2)
	*now = now.Add(keepFor)
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatal(err)
	}
	m.expect(t, 3)
}

// A host that's really gone, or moved, is looked up afresh once what was found before doesn't answer.
func TestDialerLooksAgainOnceWhatItFoundBeforeIsGone(t *testing.T) {
	// Nothing accepts, but the kernel completes a connection to a listener anyway.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	address := net.JoinHostPort("hub.local", port)
	m := &misses{}
	d, now := missingDialer(m)
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	m.miss()
	*now = now.Add(keepFor)
	err = dialOnce(t.Context(), d, address)
	if err == nil {
		t.Fatal("dialed a listener that's closed")
	}
	for _, want := range []string{"nothing answered over mDNS", "no such host", "the addresses found for it before", "127.0.0.1:" + port} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q doesn't say %q", err, want)
		}
	}
	m.expect(t, 2)
	err = dialOnce(t.Context(), d, address)
	if err == nil || strings.Contains(err.Error(), "before") {
		t.Errorf("got %v, want a lookup that finds nothing, with nothing kept to dial", err)
	}
	m.expect(t, 3)
}

// A dial given up on while it looks the host up says nothing about what was found before, so that's
// kept for the next.
func TestDialerKeepsWhatItFoundBeforeWhenADialIsGivenUp(t *testing.T) {
	address := net.JoinHostPort("hub.local", listenLoopback(t, "127.0.0.1:0"))
	m := &misses{}
	d, now := missingDialer(m)
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatal(err)
	}
	m.miss()
	*now = now.Add(keepFor)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := dialOnce(ctx, d, address); err == nil {
		t.Fatal("dialed with a context that had ended")
	}
	if err := dialOnce(t.Context(), d, address); err != nil {
		t.Fatalf("forgot what it found before: %v", err)
	}
	m.expect(t, 3)
}

// An address that never answers gets only its share of the time, so the next is still tried.
func TestDialerSplitsTheTimeBetweenAddresses(t *testing.T) {
	old := minPerAddress
	minPerAddress = 10 * time.Millisecond
	t.Cleanup(func() { minPerAddress = old })
	port := listenLoopback(t, "127.0.0.1:0")
	// 192.0.2.1 is TEST-NET-1, which nothing answers.
	d := newDialer(answers("192.0.2.1", "127.0.0.1"), failed("no such host"))
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("hub.local", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestDialerLeavesMacOSToItsResolver(t *testing.T) {
	port := listenLoopback(t, "127.0.0.1:0")
	d := newDialer(failed("asked mDNS"), failed("asked the resolver"))
	d.systemOnly = true
	// Nobody has the name, so it fails, but not by asking mDNS. The system's resolver may wait for
	// mDNS itself, so it gets only a moment.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := d.DialContext(ctx, "tcp", net.JoinHostPort("nobody.invalid.local", port))
	if err == nil || strings.Contains(err.Error(), "asked mDNS") {
		t.Errorf("got %v, want the system's own error", err)
	}
}

func TestDialerTriesTheNextAddress(t *testing.T) {
	// Only ::1 listens, so 127.0.0.1, which it tries first, refuses.
	port := listenLoopback(t, "[::1]:0")
	d := newDialer(answers("::1", "127.0.0.1"), failed("no such host"))
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
	d := newDialer(answers("127.0.0.1"), failed("no such host"))
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
	d := newDialer(failed("nothing answered over mDNS"), failed("no such host"))
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
	d := newDialer(failed("asked mDNS"), failed("asked the resolver"))
	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}
