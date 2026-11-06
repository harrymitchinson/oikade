package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/host/pluginwire"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// fakeHost serves a plugin a home imports as the host does, and carries its messages to and from bus,
// so that the checks can't tell how it's connected. It returns the socket.
func fakeHost(ctx context.Context, t T, cfg Config, bus mqtt.Bus) string {
	t.Helper()
	raw, err := os.ReadFile(cfg.Devices)
	if err != nil {
		t.Fatal(err)
	}
	var declared contractv1.DevicesFile
	if err := json.Unmarshal(raw, &declared); err != nil {
		t.Fatal(err)
	}
	// Not a test's temporary directory: a Unix socket's path has to fit in about 100 bytes.
	dir, err := os.MkdirTemp("", "oikconf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	var mu sync.Mutex
	var current *pluginwire.Conn[pluginwire.Message]
	_, err = bus.Subscribe(ctx, root.SetFilter(), func(m mqtt.Message) {
		mu.Lock()
		conn := current
		mu.Unlock()
		if conn != nil && !m.Retained {
			_ = conn.Write(pluginwire.Message{Type: pluginwire.TypePublish, Topic: m.Topic, Data: m.Payload})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	session := func(conn *pluginwire.Conn[pluginwire.Message]) {
		defer func() { _ = conn.Close() }()
		hello, err := conn.Read()
		if err != nil {
			return
		}
		if hello.Role != pluginwire.RolePlugin || hello.Plugin != cfg.Plugin || hello.Version != pluginwire.Version {
			_ = conn.Write(pluginwire.Message{Type: pluginwire.TypeRejected, Error: "not the plugin under test"})
			return
		}
		err = conn.Write(pluginwire.Message{
			Type: pluginwire.TypeWelcome, Version: pluginwire.Version, Topic: string(root), Declared: declared.Devices,
		})
		if err != nil {
			return
		}
		mu.Lock()
		current = conn
		mu.Unlock()
		for {
			m, err := conn.Read()
			if err != nil {
				break
			}
			if m.Type == pluginwire.TypePublish {
				_ = bus.Publish(ctx, m.Topic, m.Data, m.Retain)
			}
		}
		mu.Lock()
		last := current == conn
		if last {
			current = nil
		}
		mu.Unlock()
		if !last {
			return // The process's replacement is here already, and it's online.
		}
		// As the host does for a process that's gone, and a broker for a Last Will.
		offline, _ := json.Marshal(contractv1.Status{State: contractv1.Offline, Contract: contractv1.Contract})
		_ = bus.Publish(ctx, root.Status(), offline, true)
	}
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Logf("accepting a plugin: %v", err)
				}
				return
			}
			go session(pluginwire.NewConn[pluginwire.Message](c))
		}
	}()
	return socket
}
