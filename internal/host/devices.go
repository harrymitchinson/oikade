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

	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
)

// inventory keeps the live devices in step with the registry: the registry decides what exists and
// what it's called, and each driver instance sees its own devices there, by their keys.
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
	home         []wire.DeviceControl
	declarations *store.Namespace
	// declared are the keys of the devices the home last attached declares.
	declared map[string]bool
	// emit, if set, puts a forgotten device in the event log.
	emit func(api.Event)
}

// isDeclared says the home last attached declares the device whose key that is.
func (i *inventory) isDeclared(key string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.declared[key]
}

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
	return devKey(d), nil
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
			i.log.Error("Ignored unreadable arbitration settings", "err", err, "device", ident.DeviceRef(d.Key()))
		}
	}
	return hub.Meta{Room: d.Room, Instance: instance, Control: i.withHome(d, entries), DeviceTypes: d.DeviceTypes()}, true
}

// withHome is what's on record for a device, overlaid with what the home says: its own settings, and
// its room's on the capabilities it has none of its own for. Call with mu held.
func (i *inventory) withHome(d registry.Device, entries []control.Entry) []control.Entry {
	home := wire.SettingsFor(i.home, d.Key(), d.Room, d.DeviceTypes(), d.Capabilities)
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
	return devKey(d), ok
}

// sync gives each driver instance its devices. A device whose hardware changed is another key, so
// its live state starts again. Call with mu held.
func (i *inventory) sync() {
	metas := map[string]map[ident.ID]hub.Meta{}
	for name := range i.instances {
		metas[name] = map[ident.ID]hub.Meta{}
	}
	for _, d := range i.registry.List().Devices {
		if meta, ok := i.meta(d); ok {
			metas[d.Driver][devKey(d)] = meta
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

// claimed says who holds a device: every claim's source, by its ref, and whether one is at Safety.
// A device with no arbiters can still have claims on record, waiting for it.
func (i *inventory) claimed(dev ident.ID) (holders []string, safety bool) {
	status := i.engine.ControlOf(dev)
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
		for _, c := range i.engine.SavedClaims(dev) {
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

// describe says what becomes of a device on record once it's forgotten, beyond its claims.
type describe func(registry.Device) api.Forgotten

// forget drops devices from the record, by their keys, refusing them all if a Safety claim holds
// one, unless told otherwise. Their claims end with them. how, which may be nil, says what becomes
// of each, from its record as it's forgotten.
func (i *inventory) forget(ctx context.Context, keys []string, allowSafety bool, how describe) (api.Response, members) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.forgetLocked(ctx, keys, allowSafety, how)
}

// forgetPlugin forgets every device of a plugin on record, as forget does.
func (i *inventory) forgetPlugin(ctx context.Context, plugin string, allowSafety bool,
	how describe) (api.Response, members) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var keys []string
	for _, d := range i.registry.List().Devices {
		if name, ok := api.PluginOf(d); ok && name == plugin && !d.Removed {
			keys = append(keys, d.Key())
		}
	}
	if len(keys) == 0 {
		return api.Response{Error: fmt.Sprintf("no device of plugin %s is on record", plugin)}, members{}
	}
	return i.forgetLocked(ctx, keys, allowSafety, how)
}

// forgetLocked forgets devices, checking them all before it changes anything. A device that was
// forgotten has its claims ended, even if a later one couldn't be forgotten.
func (i *inventory) forgetLocked(ctx context.Context, keys []string, allowSafety bool,
	how describe) (api.Response, members) {
	keys = slices.Compact(slices.Sorted(slices.Values(keys))) // A device named twice is forgotten once.
	forgotten := map[string]bool{}
	for _, d := range i.registry.List().Devices {
		forgotten[d.Key()] = forgotten[d.Key()] || d.Removed
	}
	var refused []string
	for _, key := range keys {
		d, ok := i.registry.ByKey(key)
		switch {
		case !ok && forgotten[key]:
			refused = append(refused, fmt.Sprintf("%s is forgotten already", ident.DeviceRef(key)))
		case !ok:
			refused = append(refused, noDevice(key).Error())
		case i.declared[d.Key()]:
			// The next deploy, or start, would bring it back.
			refused = append(refused, fmt.Sprintf("%s is declared in your home's code: delete its declaration and "+
				"deploy, then forget it", ident.DeviceRef(key)))
		}
	}
	if len(refused) > 0 {
		return api.Response{Error: strings.Join(refused, "; ")}, members{}
	}
	if !allowSafety {
		var held []string
		for _, key := range keys {
			if holders, safety := i.claimed(ident.ID{Name: key}); safety {
				held = append(held, fmt.Sprintf("%s holds %s at Safety", strings.Join(holders, ", "), ident.DeviceRef(key)))
			}
		}
		if len(held) > 0 {
			return api.Response{Error: strings.Join(held, "; "), NeedsAllowSafety: true}, members{}
		}
	}
	before := i.store.Members()
	var done []api.Forgotten
	var problems []string
	for _, key := range keys {
		d, _ := i.registry.ByKey(key)
		var f api.Forgotten
		if how != nil {
			f = how(d)
		}
		f.Device, f.Ref = key, ident.DeviceRef(key).String()
		f.Ended, _ = i.claimed(ident.ID{Name: key})
		if err := i.registry.Remove(key); err != nil {
			if errors.Is(err, registry.ErrNoDevice) {
				err = noDevice(key)
			}
			problems = append(problems, err.Error())
			break
		}
		done = append(done, f)
	}
	i.sync()
	for _, f := range done {
		if err := i.engine.ForgetDevice(ctx, ident.ID{Name: f.Device}); err != nil {
			problems = append(problems, fmt.Sprintf("forgot %s, but couldn't end its claims: %v", f.Ref, err))
		}
		i.log.Info("Forgot device", "device", f.Ref)
		i.record(api.Event{Type: api.EventForget, Device: f.Device})
	}
	return api.Response{Error: strings.Join(problems, "; "), Forgotten: done},
		members{before: before, after: i.store.Members()}
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
		for _, d := range list.Devices {
			if s.devices.isDeclared(d.Key()) {
				resp.Declared = append(resp.Declared, d.Key())
			}
		}
		return resp
	case api.TypeForget:
		return s.forget(req)
	default:
		err = errors.New("unknown device request")
	}
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
}

// forget forgets devices, or every device of a plugin, says what became of each, and warns when that
// empties a room a hold reads.
func (s *apiServer) forget(req api.Request) api.Response {
	if (req.Plugin != "") == (len(req.Devices) > 0) {
		return api.Response{Error: "a forget takes devices or a plugin, one or the other"}
	}
	// What the host serves now, which only a deploy changes: what's hidden from Matter, and what's
	// imported.
	var hidden []wire.HiddenPlugin
	if s.srv.options != nil {
		hidden = s.srv.options.hiddenFromMatter()
	}
	var imported, outside []string
	known := false
	if p := s.srv.plugins; p != nil {
		imported, known = p.imports()
		outside = p.host.Outside
	}
	how := func(d registry.Device) api.Forgotten {
		plugin, ok := api.PluginOf(d)
		return api.Forgotten{
			Plugin: plugin, ComesBack: !ok,
			Unimported: ok && known && !slices.Contains(imported, plugin) && !slices.Contains(outside, plugin),
			OffMatter:  s.bridge != nil && api.Bridgeable(d) && !api.HiddenFromMatter(hidden, d),
		}
	}
	var resp api.Response
	var change members
	if req.Plugin != "" {
		resp, change = s.devices.forgetPlugin(s.ctx, req.Plugin, req.AllowSafety, how)
	} else {
		resp, change = s.devices.forget(s.ctx, req.Devices, req.AllowSafety, how)
	}
	if len(resp.Forgotten) > 0 {
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
// the read's types among before and has none among after, by room, each sorted. A
// read that was empty already isn't the change's doing. keep, if set, says which holds' rules to
// keep: nil keeps every one.
func (s *server) emptyReads(before, after map[string]hub.Member, keep func(rule string) bool) map[string][]string {
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
			if keep != nil && !keep(rule) {
				continue
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

// devKey is how the hub and the engine know a device on record.
func devKey(d registry.Device) ident.ID { return ident.ID{Name: d.Key()} }
