package host

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/store"
)

// startHost runs a host with no process to supervise, for tests that speak the protocol themselves.
func startHost(t *testing.T, dir string) (socket string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h := &Host{DataDir: dir, Log: logging.Discard(), Devices: []SimulatedDevice{
		{Name: "lamp", Room: "hall", Capabilities: []string{"on"}, Values: map[string]any{"on": false}},
	}}
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	socket = filepath.Join(dir, SocketName)
	t.Cleanup(cancel)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.Dial("unix", socket); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the host never opened its socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return socket, func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// tempDir is short: a Unix socket's path has to fit in about 100 bytes, and t.TempDir's may not.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "oik")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func dial(t *testing.T, socket string, rules ...string) (*wire.Client, wire.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, welcome, err := wire.Dial(ctx, socket, rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	go c.Listen(wire.Events{Changed: func(wire.Message) {}, Discovered: func(wire.Device) {}, Ping: func(uint64) {}})
	return c, welcome
}

func TestWelcomeListsTheDevices(t *testing.T) {
	socket, _ := startHost(t, tempDir(t))
	_, welcome := dial(t, socket)
	if len(welcome.Devices) != 1 {
		t.Fatalf("got %+v", welcome.Devices)
	}
	d := welcome.Devices[0]
	if d.Name != "lamp" || d.Room != "hall" || !d.Discovered || d.Values["on"] != false {
		t.Fatalf("got %+v", d)
	}
}

func TestHostRefusesAnotherVersionAndARole(t *testing.T) {
	socket, _ := startHost(t, tempDir(t))
	for name, hello := range map[string]wire.Message{
		"version": {Type: wire.TypeHello, Version: wire.Version + 1, Role: wire.RoleAutomations},
		// A process from before releaseAll's below would release Safety claims if it were let in.
		"old version": {Type: wire.TypeHello, Version: 1, Role: wire.RoleAutomations},
		"no version":  {Type: wire.TypeHello, Role: wire.RoleAutomations},
		"role":        {Type: wire.TypeHello, Version: wire.Version, Role: "toaster"},
		"type":        {Type: wire.TypePong},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := net.Dial("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			conn := wire.NewConn(raw)
			defer func() { _ = conn.Close() }()
			if err := conn.Write(hello); err != nil {
				t.Fatal(err)
			}
			m, err := conn.Read()
			if err != nil {
				t.Fatal(err)
			}
			if m.Type != wire.TypeRejected {
				t.Fatalf("got %+v", m)
			}
		})
	}
}

func TestSocketIsPrivate(t *testing.T) {
	socket, _ := startHost(t, tempDir(t))
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("got mode %o", mode)
	}
}

func TestHoldStateIsReturnedToTheNextSession(t *testing.T) {
	dir := tempDir(t)
	socket, stop := startHost(t, dir)
	key := ident.ID{Namespace: wire.Namespace, Name: "porch"}
	c, _ := dial(t, socket, "porch")
	want := engine.HoldState{Active: true, Latched: true, Target: engine.HoldTarget{Device: "lamp", Capability: "on", Value: true}}
	if err := c.Put(t.Context(), key, want); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get(key); !ok || !got.Latched {
		t.Fatalf("got %+v, %v", got, ok)
	}
	c.Close()

	// A new session, and then a new host, both get it back.
	for range 2 {
		deadline := time.Now().Add(5 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			next, welcome, err := wire.Dial(ctx, socket, []string{"porch"}, nil)
			cancel()
			if err == nil {
				got := welcome.Holds["porch"]
				if !got.Latched || !got.Active {
					t.Fatalf("got %+v", got)
				}
				next.Close()
				break
			}
			if !strings.Contains(err.Error(), "already connected") || time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		stop()
		socket, stop = startHost(t, dir)
	}
	stop()
}

func TestJournalEntryIsOnDiskWhenAcknowledged(t *testing.T) {
	dir := tempDir(t)
	socket, stop := startHost(t, dir)
	c, _ := dial(t, socket)
	if seq, err := c.Journal(t.Context(), "run-1", []byte("first")); err != nil || seq != 1 {
		t.Fatalf("got %d, %v", seq, err)
	}
	if seq, err := c.Journal(t.Context(), "run-1", []byte("second")); err != nil || seq != 2 {
		t.Fatalf("got %d, %v", seq, err)
	}
	stop()

	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	entries, err := data.Journal().Entries("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || string(entries[1].Data) != "second" {
		t.Fatalf("got %+v", entries)
	}
}
