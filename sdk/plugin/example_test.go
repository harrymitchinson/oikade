package plugin_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
)

// relays is a Handler for simulated relays, one per declared device.
type relays struct {
	devices []contract.DeclaredDevice
}

// Serve reports every relay as found, online and off, then follows them until ctx ends.
func (r *relays) Serve(ctx context.Context, p *plugin.Plugin) error {
	reports := make([]contract.DeviceReport, 0, len(r.devices))
	for _, d := range r.devices {
		reports = append(reports, contract.DeviceReport{ID: d.ID, Found: true, Capabilities: []string{contract.On}})
	}
	if err := p.SetDevices(ctx, reports, 0); err != nil {
		return err
	}
	for _, d := range r.devices {
		if err := p.SetAvailability(ctx, d.ID, true); err != nil {
			return err
		}
		if err := p.SetState(ctx, d.ID, contract.State{contract.On: false}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

// Set switches a relay, and reports what it is now.
func (r *relays) Set(ctx context.Context, p *plugin.Plugin, cmd contract.SetCommand) error {
	on, ok := cmd.Value.(bool)
	if !ok {
		return fmt.Errorf("%s takes a bool", cmd.Capability)
	}
	return p.SetState(ctx, cmd.ID, contract.State{contract.On: on})
}

// A plugin implements Handler and calls Run from main, which serves until the plugin is stopped.
func Example() {
	plugin.Run(func(_ plugin.Environment, devices []contract.DeclaredDevice) (plugin.Handler, error) {
		return &relays{devices: devices}, nil
	})
}

// ReadDevices reads the devices file an outside plugin is given in OIKADE_DEVICES_FILE.
func ExampleReadDevices() {
	dir, err := os.MkdirTemp("", "plugin")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "devices.json")
	err = os.WriteFile(file, []byte(`{"contract": 1, "devices": [
		{"id": "0x00158d0001a2b3c4", "name": "porch-light", "displayName": "Porch light", "room": "porch"}
	]}`), 0o600)
	if err != nil {
		panic(err)
	}
	devices, err := plugin.ReadDevices(file)
	if err != nil {
		panic(err)
	}
	for _, d := range devices {
		fmt.Println(d.ID, d.Name, d.Room)
	}
	// Output: 0x00158d0001a2b3c4 porch-light porch
}
