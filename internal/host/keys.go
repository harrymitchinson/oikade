package host

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

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

// byRule are where a host keeps state by a rule's or an average's key, which starts with its
// device's: a case's hold and latch, its timers' fires and its pause, and an average of a reading.
var byRule = []string{engine.HoldStatesNamespace, engine.FireStatesNamespace, engine.AverageStatesNamespace, pausedNamespace}

// keyByHardware starts again what a host saved under the name of a device it now keys by hardware,
// or of one reserved without hardware, which is gone, as nothing would find it: its claims, its
// cases' holds, latches, fires and pauses, the averages of its readings, and the flow runs whose
// steps name it. A latch that held lets go. A flow that names no such device keeps what it has, and
// so do the home's settings, as the next hello replaces them, but for the devices it shows on
// Matter, which it keys by hardware. The store says so once it's done, so a host that stopped part
// way does it again.
func keyByHardware(data *store.Store, log *slog.Logger) error {
	names, err := registry.Names(data.Namespace(registry.Namespace))
	if err != nil {
		return err
	}
	// A host that keyed the rest already still had names in its registry, and until a home built for
	// this one connects, the devices shown by name would be off Matter.
	if err := keyShownByHardware(data.Namespace(optionsNamespace), names); err != nil {
		return err
	}
	layout := data.Namespace(layoutNamespace)
	raw, ok, err := layout.Get(deviceKeys)
	if err != nil {
		return fmt.Errorf("reading how the store keys devices: %w", err)
	}
	if ok && string(raw) == keyedByHardware {
		return nil
	}
	named := map[string]bool{}
	for name := range names {
		named[name] = true
	}
	var states, runs int
	if len(named) > 0 {
		if states, err = dropNamed(data, named); err != nil {
			return err
		}
		if runs, err = dropRunsNaming(data.Journal(), named); err != nil {
			return err
		}
	}
	if states+runs > 0 {
		log.Info("Started claims, latches and flow runs again, as the store kept them by device name",
			"states", states, "runs", runs)
	}
	if err := layout.Put(deviceKeys, []byte(keyedByHardware)); err != nil {
		return fmt.Errorf("saving how the store keys devices: %w", err)
	}
	return nil
}

// dropNamed deletes the claims of the named devices, the state of their rules and averages, and that
// of a hold that writes one, and says how many it deleted.
func dropNamed(data *store.Store, named map[string]bool) (int, error) {
	dropped := 0
	drop := func(namespace string, of func(key string, value []byte) bool) error {
		values := data.Namespace(namespace)
		keys, err := values.Keys()
		if err != nil {
			return fmt.Errorf("reading %s: %w", namespace, err)
		}
		for _, k := range keys {
			raw, _, err := values.Get(k)
			if err != nil {
				return fmt.Errorf("reading %s: %w", namespace, err)
			}
			if !of(k, raw) {
				continue
			}
			if err := values.Delete(k); err != nil {
				return fmt.Errorf("starting %s again: %w", namespace, err)
			}
			dropped++
		}
		return nil
	}
	if err := drop(engine.ControlStatesNamespace, func(k string, _ []byte) bool { return named[k] }); err != nil {
		return dropped, err
	}
	for _, namespace := range byRule {
		err := drop(namespace, func(k string, raw []byte) bool {
			if device, _, _ := strings.Cut(k, " "); named[device] {
				return true
			}
			var hold engine.HoldState
			if namespace != engine.HoldStatesNamespace || json.Unmarshal(raw, &hold) != nil {
				return false
			}
			t := hold.Target
			return named[t.Device] || slices.ContainsFunc(t.Devices, func(d string) bool { return named[d] }) ||
				slices.ContainsFunc(t.Except, func(d string) bool { return named[d] })
		})
		if err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

// dropRunsNaming deletes the flow runs whose journal names one of the devices, which their steps
// would no longer match, and then the copy a deploy made, which would bring them back. It says how
// many it deleted.
func dropRunsNaming(journal *store.Journal, named map[string]bool) (int, error) {
	names := make([]string, 0, len(named))
	for n := range named {
		names = append(names, regexp.QuoteMeta(n))
	}
	slices.Sort(names)
	// A name is a slug, so a letter, digit or hyphen beside it makes it part of another word.
	mentions := regexp.MustCompile(`(?:^|[^a-z0-9-])(?:` + strings.Join(names, "|") + `)(?:[^a-z0-9-]|$)`)
	runs, err := journal.Runs()
	if err != nil {
		return 0, fmt.Errorf("reading flow runs: %w", err)
	}
	dropped := 0
	for _, run := range runs {
		entries, err := journal.Entries(run)
		if err != nil {
			return dropped, fmt.Errorf("reading flow run %s: %w", run, err)
		}
		if !slices.ContainsFunc(entries, func(e store.Entry) bool { return mentions.Match(e.Data) }) {
			continue
		}
		if err := journal.Delete(run); err != nil {
			return dropped, fmt.Errorf("dropping flow run %s: %w", run, err)
		}
		dropped++
	}
	if dropped > 0 {
		if err := journal.DiscardCopy(); err != nil {
			return dropped, fmt.Errorf("dropping the copy of the flow runs: %w", err)
		}
	}
	return dropped, nil
}
