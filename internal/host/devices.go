package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/internal/store"
)

// inventory keeps the live devices in step with the registry: the registry decides what exists and
// what it's called, and each driver instance sees its own devices there.
type inventory struct {
	registry *registry.Registry
	settings *store.Namespace
	store    *hub.Store
	engine   *engine.Engine
	log      *slog.Logger
	// changed hear that the list changed. They must not block.
	changed []func()

	// mu orders changes, so that the hub takes them up in the registry's order.
	mu        sync.Mutex
	instances map[string]driver.InstanceKey
	// home is the device settings your home declares, which win over any on record.
	home []wire.DeviceControl
	// declarations keeps the devices your home declares, from declaredNamespace.
	declarations *store.Namespace
	// renamed, if set, moves what's on record for cases and averages to the keys its function gives
	// them, before the registry renames their devices.
	renamed func(rename func(string) string) error
	// emit, if set, puts a forgotten device and a renamed one in the event log.
	emit func(api.Event)
}

// record puts an event in the event log, if there is one.
func (i *inventory) record(e api.Event) {
	if i.emit != nil {
		e.Time = time.Now()
		i.emit(e)
	}
}

// homeSettingsNamespace keeps the device settings of the home last attached.
const (
	homeSettingsNamespace = "home-settings"
	homeSettingsKey       = "control"
)

func newInventory(reg *registry.Registry, settings, declarations *store.Namespace, devices *hub.Store, eng *engine.Engine,
	log *slog.Logger,
) *inventory {
	return &inventory{
		registry: reg, settings: settings, declarations: declarations, store: devices, engine: eng, log: log,
		instances: map[string]driver.InstanceKey{},
	}
}

// instanceName is how the registry records a driver instance.
func instanceName(instance driver.InstanceKey) string {
	return instance.Driver + "/" + instance.Name
}

// onRecord counts the devices on record with hardware, whether or not their driver has reported.
func (i *inventory) onRecord() int {
	n := 0
	for _, d := range i.registry.List().Devices {
		if !d.Removed && d.ID != "" {
			n++
		}
	}
	return n
}

// attach adds a driver instance, whose devices on record are known from now on, reported or not.
func (i *inventory) attach(instance driver.InstanceKey) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.instances[instanceName(instance)] = instance
	i.sync()
}

// paired records hardware a driver reports. It's in the registry and usable when this returns.
func (i *inventory) paired(instance driver.InstanceKey, app driver.AppDevice,
	entries []control.Entry,
) (ident.ID, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var settings json.RawMessage
	if len(entries) > 0 {
		raw, err := json.Marshal(entries)
		if err != nil {
			return ident.ID{}, err
		}
		settings = raw
	}
	d, changed, err := i.registry.Report(registry.Report{
		ID: app.ID, Driver: instanceName(instance), Name: app.Name, Room: app.Room,
		Capabilities: app.Capabilities, DeviceType: app.DeviceType, Control: settings,
		Product: app.Product,
	})
	if err != nil {
		return ident.ID{}, err
	}
	if changed {
		i.sync()
	}
	return key(d.Name), nil
}

// onHub reports whether the hub keeps d: hardware on record, of a plugin attached.
func (i *inventory) onHub(d registry.Device) bool {
	_, ok := i.instances[d.Driver]
	return ok && !d.Removed && d.ID != ""
}

// meta is what the hub keeps for a device on record.
func (i *inventory) meta(d registry.Device) (hub.Meta, bool) {
	if !i.onHub(d) {
		return hub.Meta{}, false
	}
	instance := i.instances[d.Driver]
	var entries []control.Entry
	if len(d.Control) > 0 {
		if err := json.Unmarshal(d.Control, &entries); err != nil {
			i.log.Error("Ignored unreadable arbitration settings", "err", err, "device", d.Name)
		}
	}
	return hub.Meta{
		DisplayName: d.Name, Room: d.Room, Instance: instance, Control: i.withHome(d, entries), DeviceTypes: d.DeviceTypes(),
	}, true
}

// withHome is what's on record for a device, overlaid with what the home says: its own settings, and
// its room's on the capabilities it has none of its own for. Call with mu held.
func (i *inventory) withHome(d registry.Device, entries []control.Entry) []control.Entry {
	home := wire.SettingsFor(i.home, d.Name, d.Room, d.DeviceTypes(), d.Capabilities)
	var out []control.Entry
	for _, e := range entries {
		if !slices.ContainsFunc(home, func(h control.Entry) bool { return h.Capability == e.Capability }) {
			out = append(out, e)
		}
	}
	return append(out, home...)
}

// loadHome takes up the settings saved by the last process to attach. They're there before any
// process is: arbitration, Safety claims included, doesn't wait for your code to start.
func (i *inventory) loadHome() error {
	raw, ok, err := i.settings.Get(homeSettingsKey)
	if err != nil || !ok {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return json.Unmarshal(raw, &i.home)
}

// takeHome takes up the settings of a process that's connecting, which adds and changes them. It
// removes none: that ends claims, and this version hasn't proved itself. setHome does, once it has.
func (i *inventory) takeHome(settings []wire.DeviceControl) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	merged := slices.Clone(settings)
	for _, c := range i.home {
		if !slices.ContainsFunc(settings, func(n wire.DeviceControl) bool {
			return n.Device == c.Device && n.Room == c.Room && n.Entry.Capability == c.Entry.Capability
		}) {
			merged = append(merged, c)
		}
	}
	return i.saveHome(merged)
}

// setHome replaces the device settings the home declares.
func (i *inventory) setHome(settings []wire.DeviceControl) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.saveHome(settings)
}

// saveHome saves settings first, so a host that restarts arbitrates what the running code said. Call
// with mu held.
func (i *inventory) saveHome(settings []wire.DeviceControl) error {
	if slices.EqualFunc(i.home, settings, sameSetting) {
		return nil
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := i.settings.Put(homeSettingsKey, raw); err != nil {
		return err
	}
	i.home = settings
	i.sync()
	return nil
}

func sameSetting(a, b wire.DeviceControl) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && bytes.Equal(x, y)
}

// key finds a driver's device in the hub.
func (i *inventory) key(instance driver.InstanceKey, id string) (ident.ID, bool) {
	d, ok := i.registry.ByID(instanceName(instance), id)
	return key(d.Name), ok
}

// sync gives each driver instance its devices. Call with mu held.
func (i *inventory) sync() { i.syncExcept(nil) }

// syncExcept is sync, without the devices named in skip. Call with mu held.
func (i *inventory) syncExcept(skip []string) {
	metas := map[string]map[ident.ID]hub.Meta{}
	for name := range i.instances {
		metas[name] = map[ident.ID]hub.Meta{}
	}
	for _, d := range i.registry.List().Devices {
		if slices.Contains(skip, d.Name) {
			continue
		}
		if meta, ok := i.meta(d); ok {
			metas[d.Driver][key(d.Name)] = meta
		}
	}
	for name, devices := range metas {
		i.store.SetDevices(i.instances[name], devices)
	}
	for _, changed := range i.changed {
		changed()
	}
}

// announce tells your process about a device that changed.
func (i *inventory) announce(dev ident.ID) {
	if snap, ok := i.store.Get(dev); ok && snap.Discovered {
		i.store.Discovered(dev, snap.Info)
	}
}

// claimed says who holds a device: every claim's source, by its ref, and whether one is at Safety. A device with no
// arbiters can still have claims on record, waiting for it.
func (i *inventory) claimed(name string) (holders []string, safety bool) {
	status := i.engine.ControlOf(key(name))
	for _, c := range status {
		for _, o := range c.Claims {
			if o.Reverted {
				continue
			}
			holders = append(holders, control.SourceRef(o.Source).String())
			safety = safety || o.Level == control.NameSafety
		}
	}
	if len(status) == 0 {
		for _, c := range i.engine.SavedClaims(key(name)) {
			holders = append(holders, control.SourceRef(c.Source).String())
			safety = safety || c.Level == control.Safety
		}
	}
	slices.Sort(holders)
	return slices.Compact(holders), safety
}

// members are the hub's devices, by name, before and after a change to the registry, or one a deploy
// would make. Both are read with mu held, so no other change is taken for it.
type members struct {
	before, after map[string]hub.Member
}

// moveLive takes the registry's new names up in the hub, as one change. Call with mu held, after the
// registry has changed.
func (i *inventory) moveLive(names map[string]string) {
	moves := map[ident.ID]hub.Move{}
	for from, to := range names {
		d, ok := i.registry.Get(to)
		if !ok {
			continue
		}
		if meta, ok := i.meta(d); ok {
			moves[key(from)] = hub.Move{To: key(to), Meta: meta}
		}
	}
	i.store.Rename(moves)
	i.sync()
}

// remove refuses a device a Safety claim holds, unless told otherwise. Its claims end with it, so
// they can't wait for the next device of that name.
func (i *inventory) remove(ctx context.Context, name string, allowSafety bool) (api.Response, members) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.removeLocked(ctx, []string{name}, allowSafety)
}

// removePlugin removes every device of a plugin on record, as remove does one.
func (i *inventory) removePlugin(ctx context.Context, plugin string, allowSafety bool) (api.Response, members) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var names []string
	for _, d := range i.registry.List().Devices {
		if name, ok := api.PluginOf(d); ok && name == plugin && !d.Removed {
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return api.Response{Error: fmt.Sprintf("no device of plugin %s is on record", plugin)}, members{}
	}
	return i.removeLocked(ctx, names, allowSafety)
}

// removeLocked removes devices, refusing them all if a Safety claim holds one. A device that was
// removed has its claims ended, even if a later one couldn't be removed.
func (i *inventory) removeLocked(ctx context.Context, names []string, allowSafety bool) (api.Response, members) {
	for _, name := range names {
		if d, ok := i.registry.Get(name); ok && d.Declared {
			// The next deploy, or start, would bring it back.
			return api.Response{Error: fmt.Sprintf("%s is declared in your home's code: delete its declaration and deploy, "+
				"then remove it under its app's name", name)}, members{}
		}
	}
	if !allowSafety {
		var held []string
		for _, name := range names {
			if holders, safety := i.claimed(name); safety {
				held = append(held, fmt.Sprintf("%s holds %s at Safety", strings.Join(holders, ", "), name))
			}
		}
		if len(held) > 0 {
			return api.Response{Error: strings.Join(held, "; "), NeedsAllowSafety: true}, members{}
		}
	}
	before := i.store.Members()
	var removed, problems []string
	for _, name := range names {
		if err := i.registry.Remove(name); err != nil {
			if errors.Is(err, registry.ErrNoDevice) {
				err = noDevice(name)
			}
			problems = append(problems, err.Error())
			break
		}
		removed = append(removed, name)
	}
	i.sync()
	for _, name := range removed {
		if err := i.engine.ForgetDevice(ctx, key(name)); err != nil {
			problems = append(problems, fmt.Sprintf("removed %s, but couldn't end its claims: %v", name, err))
		}
		i.log.Info("Removed device", "device", name)
		i.record(api.Event{Type: api.EventForget, Device: name})
	}
	return api.Response{Error: strings.Join(problems, "; "), Removed: removed}, members{before: before, after: i.store.Members()}
}

// device answers the commands that read and change the registry.
func (s *apiServer) device(req api.Request) api.Response {
	var err error
	switch req.Type {
	case api.TypeDevices:
		list := s.devices.registry.List()
		resp := api.Response{Registry: &list}
		if p := s.srv.plugins; p != nil {
			resp.Unimported, resp.OutsidePlugins = p.unimportedNow(true), slices.Clone(p.host.Outside)
			resp.SafetyHeld = p.safetyHeld(list)
		}
		resp.HiddenFromMatter = api.HiddenDevices(list, s.srv.options.hiddenFromMatter())
		for dev, snap := range s.srv.devices.List() {
			problem := snap.Info.Error
			if snap.Online != nil && !*snap.Online {
				// Offline is what's wrong first, and the driver's error may say why (OIK-340).
				problem = strings.TrimSuffix("offline: "+problem, ": ")
			}
			if problem != "" {
				if resp.Problems == nil {
					resp.Problems = map[string]string{}
				}
				resp.Problems[dev.Name] = problem
			}
		}
		return resp
	case api.TypeDeviceRemove:
		return s.remove(req)
	default:
		err = errors.New("unknown device request")
	}
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
}

// remove removes a device, or every device of a plugin, and warns when that empties a room a hold
// reads.
func (s *apiServer) remove(req api.Request) api.Response {
	if req.Plugin != "" && req.Device != "" {
		return api.Response{Error: "a remove takes a device or a plugin, not both"}
	}
	var resp api.Response
	var change members
	if req.Plugin != "" {
		resp, change = s.devices.removePlugin(s.ctx, req.Plugin, req.AllowSafety)
	} else {
		resp, change = s.devices.remove(s.ctx, req.Device, req.AllowSafety)
	}
	if len(resp.Removed) > 0 {
		resp.Warning = emptiedWarning(s.srv.emptyReads(change.before, change.after, nil))
	}
	return resp
}

// emptiedWarning says which rooms a change to the registry left with no devices a hold reads there,
// and which holds, or is empty if it left none.
func emptiedWarning(emptied map[string][]string) string {
	rooms := slices.Sorted(maps.Keys(emptied))
	warnings := make([]string, 0, len(rooms))
	for _, room := range rooms {
		warnings = append(warnings, fmt.Sprintf("%s has no devices now that %s read, so they keep their state "+
			"until one is back in %s", room, strings.Join(emptied[room], ", "), room))
	}
	return strings.Join(warnings, "; ")
}

// emptyReads are the refs of the running process's holds with a read of a room that had devices of
// the read's types among before and has none among after, by room, each sorted. A read that was empty
// already isn't the change's doing. rename gives the key a hold's rule will have, or false for one
// to leave out; nil keeps every one as it is.
func (s *server) emptyReads(before, after map[string]hub.Member, rename func(rule string) (string, bool)) map[string][]string {
	covers := func(members map[string]hub.Member, r engine.RoomRead) bool {
		for _, m := range members {
			if m.Room == r.Room && m.OfTypes(r.Types) {
				return true
			}
		}
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	if sess := s.session; sess != nil {
		for rule, reads := range sess.reads {
			if rename != nil {
				var ok bool
				if rule, ok = rename(rule); !ok {
					continue
				}
			}
			rooms := map[string]bool{}
			for _, r := range reads {
				if r.Room != "" && covers(before, r) && !covers(after, r) {
					rooms[r.Room] = true
				}
			}
			for room := range rooms {
				out[room] = append(out[room], control.RuleRef(rule).String())
			}
		}
	}
	for _, rules := range out {
		slices.Sort(rules)
	}
	return out
}
