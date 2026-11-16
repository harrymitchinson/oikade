package host

import (
	"context"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

// A process that has gone leaves nothing to say whether it had a Safety rule, so a rename or a new
// device type asks for --allow-safety then, as a pause does. A host that has never run any code has
// none to protect.
func TestARenameWithNoProcessNeedsAllowSafetyOnceCodeHasRun(t *testing.T) {
	dir := tempDir(t)
	socket, _ := startHost(t, dir)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local := api.Local{DataDir: dir}
	for { // The API's socket may come up after the process's.
		if _, err := api.Call(ctx, local, api.Request{Type: api.TypeStatus}); err == nil {
			break
		} else if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	call := func(req api.Request) api.Response {
		t.Helper()
		resp, err := api.Call(ctx, local, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	rename := func(from, to string, allow bool) api.Response {
		t.Helper()
		return call(api.Request{Type: api.TypeDeviceName, Device: from, To: to, AllowSafety: allow})
	}
	retype := func(name string, to devicetype.Type, allow bool) api.Response {
		t.Helper()
		return call(api.Request{Type: api.TypeDeviceType, Device: name, DeviceType: to, AllowSafety: allow})
	}

	untilGone := func() {
		t.Helper()
		for {
			if resp := call(api.Request{Type: api.TypeStatus}); !resp.Status.Process {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	if resp := rename("lamp", "lamp-two", false); resp.Error != "" {
		t.Fatalf("a host that has run nothing refused a rename: %+v", resp)
	}
	if resp := retype("lamp-two", "OnOffLight", false); resp.Error != "" {
		t.Fatalf("a host that has run nothing refused a device type: %+v", resp)
	}

	// A process with no rules says so, and protects nothing.
	empty, _, err := wire.Dial(ctx, socket, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go empty.Listen(wire.Events{Changed: func(wire.Message) {}, Discovered: func(wire.Device) {}, Ping: func(uint64) {}})
	if resp := rename("lamp-two", "lamp-four", false); resp.Error != "" {
		t.Fatalf("a process with no rules refused a rename: %+v", resp)
	}
	if resp := rename("lamp-four", "lamp-two", false); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	empty.Close()
	untilGone()

	c, _, err := wire.Dial(ctx, socket, []string{"leak"}, nil, wire.RuleInfo{Name: "leak", Kind: wire.KindCase, Safety: true})
	if err != nil {
		t.Fatal(err)
	}
	go c.Listen(wire.Events{Changed: func(wire.Message) {}, Discovered: func(wire.Device) {}, Ping: func(uint64) {}})
	if resp := rename("lamp-two", "lamp-three", false); !resp.NeedsAllowSafety {
		t.Fatalf("renamed with a Safety rule running: %+v", resp)
	}
	c.Close()
	untilGone()

	if resp := rename("lamp-two", "lamp-three", false); !resp.NeedsAllowSafety {
		t.Fatalf("renamed with no way to tell whether a Safety rule runs: %+v", resp)
	}
	if resp := retype("lamp-two", "", false); !resp.NeedsAllowSafety {
		t.Fatalf("changed a device type with no way to tell whether a Safety rule writes to it: %+v", resp)
	}
	if resp := rename("lamp-two", "lamp-three", true); resp.Error != "" {
		t.Fatalf("with --allow-safety: %+v", resp)
	}
	if resp := retype("lamp-three", "", true); resp.Error != "" {
		t.Fatalf("with --allow-safety: %+v", resp)
	}
}
