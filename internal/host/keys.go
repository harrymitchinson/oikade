package host

import (
	"fmt"
	"log/slog"

	"oikade.dev/internal/engine"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
)

// layoutNamespace says how the host keys what it saves for a device: deviceKeys is keyedByHardware
// once it keys it by the device's hardware, as registry.Device.Key says.
const (
	layoutNamespace = "layout"
	deviceKeys      = "deviceKeys"
	keyedByHardware = "hardware"
)

// keyedByName are where a host kept state under a device's name, in its key or in what it keeps:
// claims, a case's hold and latch, timers' fires, averages, pauses and the home's device settings.
var keyedByName = []string{
	engine.ControlStatesNamespace, engine.HoldStatesNamespace, engine.FireStatesNamespace,
	engine.AverageStatesNamespace, pausedNamespace, homeSettingsNamespace,
}

// keyByHardware starts again what a host saved under devices' names, which nothing finds by its
// hardware: that state, and the flow runs, whose steps name devices too. A latch that held lets go.
// The store says so once it's done, so a host that stopped part way does it again. A store with no
// devices on record is new, or kept nothing a device has.
func keyByHardware(data *store.Store, log *slog.Logger) error {
	layout := data.Namespace(layoutNamespace)
	raw, ok, err := layout.Get(deviceKeys)
	if err != nil {
		return fmt.Errorf("reading how the store keys devices: %w", err)
	}
	if ok && string(raw) == keyedByHardware {
		return nil
	}
	recorded, err := data.Namespace(registry.Namespace).Keys()
	if err != nil {
		return fmt.Errorf("reading the devices on record: %w", err)
	}
	if len(recorded) == 0 {
		return layout.Put(deviceKeys, []byte(keyedByHardware))
	}
	dropped := 0
	for _, name := range keyedByName {
		values := data.Namespace(name)
		keys, err := values.Keys()
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		for _, k := range keys {
			if err := values.Delete(k); err != nil {
				return fmt.Errorf("starting %s again: %w", name, err)
			}
		}
		dropped += len(keys)
	}
	journal := data.Journal()
	runs, err := journal.Runs()
	if err != nil {
		return fmt.Errorf("reading flow runs: %w", err)
	}
	for _, run := range runs {
		if err := journal.Delete(run); err != nil {
			return fmt.Errorf("dropping flow run %s: %w", run, err)
		}
	}
	if err := journal.DiscardCopy(); err != nil {
		return fmt.Errorf("dropping the copy of the flow runs: %w", err)
	}
	if dropped+len(runs) > 0 {
		log.Info("Started claims, latches and flow runs again, as the store kept them by device name",
			"states", dropped, "runs", len(runs))
	}
	if err := layout.Put(deviceKeys, []byte(keyedByHardware)); err != nil {
		return fmt.Errorf("saving how the store keys devices: %w", err)
	}
	return nil
}
