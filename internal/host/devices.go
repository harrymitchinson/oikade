package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
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
}

// homeSettingsNamespace keeps the device settings of the home last attached.
const (
	homeSettingsNamespace = "home-settings"
	homeSettingsKey       = "control"
)

func newInventory(reg *registry.Registry, settings *store.Namespace, devices *hub.Store, eng *engine.Engine, log *slog.Logger) *inventory {
	return &inventory{
		registry: reg, settings: settings, store: devices, engine: eng, log: log, instances: map[string]driver.InstanceKey{},
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
	})
	if err != nil {
		return ident.ID{}, err
	}
	if changed {
		i.sync()
	}
	return key(d.Name), nil
}

// meta is what the hub keeps for a device on record.
func (i *inventory) meta(d registry.Device) (hub.Meta, bool) {
	instance, ok := i.instances[d.Driver]
	if !ok || d.Removed || d.ID == "" {
		return hub.Meta{}, false
	}
	var entries []control.Entry
	if len(d.Control) > 0 {
		if err := json.Unmarshal(d.Control, &entries); err != nil {
			i.log.Error("Ignored unreadable arbitration settings", "err", err, "device", d.Name)
		}
	}
	return hub.Meta{
		DisplayName: d.Name, Room: d.Room, Instance: instance, Control: i.withHome(d.Name, entries), DeviceTypes: d.DeviceTypes(),
	}, true
}

// withHome is what's on record for a device, overlaid with what the home says. Call with mu held.
func (i *inventory) withHome(name string, entries []control.Entry) []control.Entry {
	var out []control.Entry
	for _, e := range entries {
		if !slices.ContainsFunc(i.home, func(c wire.DeviceControl) bool {
			return c.Device == name && c.Entry.Capability == e.Capability
		}) {
			out = append(out, e)
		}
	}
	for _, c := range i.home {
		if c.Device == name {
			out = append(out, c.Entry)
		}
	}
	return out
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
			return n.Device == c.Device && n.Entry.Capability == c.Entry.Capability
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
func (i *inventory) sync() {
	metas := map[string]map[ident.ID]hub.Meta{}
	for name := range i.instances {
		metas[name] = map[ident.ID]hub.Meta{}
	}
	for _, d := range i.registry.List().Devices {
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

// claimed says who holds a device: every claim, and whether one is at Safety. A device with no
// arbiters can still have claims on record, waiting for it.
func (i *inventory) claimed(name string) (holders []string, safety bool) {
	status := i.engine.ControlOf(key(name))
	for _, c := range status {
		for _, o := range c.Claims {
			if o.Reverted {
				continue
			}
			holders = append(holders, o.Source)
			safety = safety || o.Level == control.NameSafety
		}
	}
	if len(status) == 0 {
		for _, c := range i.engine.SavedClaims(key(name)) {
			holders = append(holders, c.Source)
			safety = safety || c.Level == control.Safety
		}
	}
	slices.Sort(holders)
	return slices.Compact(holders), safety
}

func (i *inventory) refuseClaimed(name, doing string) error {
	if holders, _ := i.claimed(name); len(holders) > 0 {
		return fmt.Errorf("%s holds %s, so it can't be %s: claims stay with a name", strings.Join(holders, ", "), name, doing)
	}
	return nil
}

func (i *inventory) rename(name, to string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.refuseClaimed(name, "renamed"); err != nil {
		return err
	}
	if err := i.registry.Rename(name, to); err != nil {
		return err
	}
	i.moveLive(map[string]string{name: to})
	i.announce(key(to))
	i.log.Info("Renamed device", "device", name, "to", to)
	return nil
}

// move puts a device in a room, and returns the room it was in.
func (i *inventory) move(name, room string) (from string, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if d, ok := i.registry.Get(name); ok {
		from = d.Room
	}
	if err := i.registry.Move(name, room); err != nil {
		return "", err
	}
	i.sync()
	i.announce(key(name))
	i.log.Info("Moved device", "device", name, "room", room)
	return from, nil
}

// setType says what a device is. The Matter bridge rebuilds its endpoint, on the same ID.
func (i *inventory) setType(name string, t devicetype.Type) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.registry.SetDeviceType(name, t); err != nil {
		return err
	}
	i.sync()
	i.announce(key(name))
	d, _ := i.registry.Get(name)
	i.log.Info("Set device type", "device", name, "deviceType", t, "deviceTypes", d.DeviceTypes())
	return nil
}

func (i *inventory) reserve(name, room string) error {
	if err := i.registry.Reserve(name, room); err != nil {
		return err
	}
	i.log.Info("Reserved device name", "device", name)
	return nil
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
func (i *inventory) remove(ctx context.Context, name string, allowSafety bool) api.Response {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.removeLocked(ctx, []string{name}, allowSafety)
}

// removePlugin removes every device of a plugin on record, as remove does one.
func (i *inventory) removePlugin(ctx context.Context, plugin string, allowSafety bool) api.Response {
	i.mu.Lock()
	defer i.mu.Unlock()
	var names []string
	for _, d := range i.registry.List().Devices {
		if name, ok := api.PluginOf(d); ok && name == plugin && !d.Removed {
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return api.Response{Error: fmt.Sprintf("no device of plugin %s is on record", plugin)}
	}
	return i.removeLocked(ctx, names, allowSafety)
}

// removeLocked removes devices, refusing them all if a Safety claim holds one. A device that was
// removed has its claims ended, even if a later one couldn't be removed.
func (i *inventory) removeLocked(ctx context.Context, names []string, allowSafety bool) api.Response {
	if !allowSafety {
		var held []string
		for _, name := range names {
			if holders, safety := i.claimed(name); safety {
				held = append(held, fmt.Sprintf("%s holds %s at Safety", strings.Join(holders, ", "), name))
			}
		}
		if len(held) > 0 {
			return api.Response{Error: strings.Join(held, "; "), NeedsAllowSafety: true}
		}
	}
	var removed, problems []string
	for _, name := range names {
		if err := i.registry.Remove(name); err != nil {
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
	}
	return api.Response{Error: strings.Join(problems, "; "), Removed: removed}
}

// replace moves name onto with's hardware. name's claims and arbitration settings stay, and with's
// live values come across.
func (i *inventory) replace(name, with string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.refuseClaimed(with, "moved"); err != nil {
		return err
	}
	if err := i.registry.Replace(name, with); err != nil {
		return err
	}
	i.moveLive(map[string]string{with: name})
	i.announce(key(name))
	i.log.Info("Moved device name onto new hardware", "device", name, "from", with)
	return nil
}

// restore refuses a copy that would take a claimed name from its hardware, since claims stay with a
// name. After a real loss nothing is claimed, because claims are in the same store.
func (i *inventory) restore(from registry.Snapshot, force bool) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	before := i.registry.List()
	for _, d := range before.Devices {
		if d.Removed {
			continue
		}
		holders, _ := i.claimed(d.Name)
		if len(holders) > 0 && restoreMoves(from, d) {
			return nil, fmt.Errorf("%s holds %s, and restoring would move that name", strings.Join(holders, ", "), d.Name)
		}
	}
	moved, err := i.registry.Restore(from, force)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, d := range before.Devices {
		if d.Removed || d.ID == "" {
			continue
		}
		if now, ok := i.registry.ByID(d.Driver, d.ID); ok && now.Name != d.Name {
			names[d.Name] = now.Name
		}
	}
	i.moveLive(names)
	for _, d := range i.registry.List().Devices {
		if !d.Removed {
			i.announce(key(d.Name))
		}
	}
	i.log.Info("Restored device registry", "devices", len(from.Devices), "moved", len(moved))
	return moved, nil
}

// restoreMoves says a copy would rename or remove a device, or give its name to other hardware.
func restoreMoves(from registry.Snapshot, d registry.Device) bool {
	for _, f := range from.Devices {
		same := d.ID != "" && f.ID == d.ID && f.Driver == d.Driver
		if same && (f.Name != d.Name || f.Removed) {
			return true
		}
		if !same && !f.Removed && f.Name == d.Name {
			return true
		}
	}
	return false
}

// typedOut names the running code's Safety rules that a device would drop out of with device type t:
// a room write limited to types it's one of now and wouldn't be.
func (s *apiServer) typedOut(name string, t devicetype.Type) []string {
	d, ok := s.devices.registry.Get(name)
	if !ok {
		return nil
	}
	before := d.DeviceTypes()
	d.DeviceType = t
	after := d.DeviceTypes()
	rules, _ := s.rulesInfo()
	var out []string
	for _, r := range rules {
		if !r.Safety {
			continue
		}
		if slices.ContainsFunc(r.Rooms, func(room wire.RoomOfTypes) bool {
			if room.Room != d.Room || !slices.Contains(d.Capabilities, room.Capability) {
				return false
			}
			return devicetype.Any(before, room.DeviceTypes) && !devicetype.Any(after, room.DeviceTypes)
		}) {
			out = append(out, r.Name)
		}
	}
	return out
}

// safetyRule names a Safety rule of the running code, if there is one.
func (s *apiServer) safetyRule() string {
	rules, _ := s.rulesInfo()
	for _, r := range rules {
		if r.Safety {
			return r.Name
		}
	}
	return ""
}

// device answers the commands that read and change the registry. Running code loses track of a
// device whose name changes until a deploy, which would silently stop a Safety rule naming it.
func (s *apiServer) device(req api.Request) api.Response {
	if req.Type == api.TypeDeviceName || req.Type == api.TypeDeviceReplace {
		if rule := s.safetyRule(); rule != "" && !req.AllowSafety {
			return api.Response{
				Error: fmt.Sprintf("%s is a Safety rule, and running code loses track of %s until you deploy, "+
					"so it may stop protecting anything", rule, req.Device),
				NeedsAllowSafety: true,
			}
		}
	}
	var err error
	switch req.Type {
	case api.TypeDevices:
		list := s.devices.registry.List()
		resp := api.Response{Registry: &list}
		if p := s.srv.plugins; p != nil {
			resp.Unimported, resp.OutsidePlugins = p.unimportedNow(true), slices.Clone(p.host.Outside)
			resp.SafetyHeld = p.safetyHeld(list)
		}
		return resp
	case api.TypeDeviceName:
		err = s.devices.rename(req.Device, req.To)
	case api.TypeDeviceRoom:
		return s.move(req.Device, req.Room)
	case api.TypeDeviceType:
		if rules := s.typedOut(req.Device, req.DeviceType); len(rules) > 0 && !req.AllowSafety {
			return api.Response{
				Error: fmt.Sprintf("%s would no longer be one of the device types %s writes at Safety, so it would stop "+
					"protecting it", req.Device, strings.Join(rules, ", ")),
				NeedsAllowSafety: true,
			}
		}
		err = s.devices.setType(req.Device, req.DeviceType)
	case api.TypeDeviceReserve:
		err = s.devices.reserve(req.Device, req.Room)
	case api.TypeDeviceRemove:
		if req.Plugin != "" {
			if req.Device != "" {
				return api.Response{Error: "a remove takes a device or a plugin, not both"}
			}
			return s.devices.removePlugin(s.ctx, req.Plugin, req.AllowSafety)
		}
		return s.devices.remove(s.ctx, req.Device, req.AllowSafety)
	case api.TypeDeviceReplace:
		err = s.devices.replace(req.Device, req.To)
	case api.TypeDeviceRestore:
		if req.Registry == nil {
			return api.Response{Error: "a restore needs a registry"}
		}
		moved, err := s.devices.restore(*req.Registry, req.Force)
		if err != nil {
			return api.Response{Error: err.Error(), NeedsForce: errors.Is(err, registry.ErrDisagrees)}
		}
		return api.Response{Moved: moved}
	default:
		err = errors.New("unknown device request")
	}
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	return api.Response{}
}

// move moves a device to a room. It warns when that leaves the room it was in with no devices a hold
// reads there: the hold's when stops, and it keeps its state until one is back.
func (s *apiServer) move(name, room string) api.Response {
	from, err := s.devices.move(name, room)
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	if from == "" || from == room {
		return api.Response{}
	}
	rules := s.emptyReads(name, from, s.devices.store.Members())
	if len(rules) == 0 {
		return api.Response{}
	}
	return api.Response{Warning: fmt.Sprintf("%s has no devices now that %s read, so they keep their state "+
		"until one is back in %s", from, strings.Join(rules, ", "), from)}
}

// emptyReads are the holds of the running process whose read of room moved took from it the last of
// the devices it read there, among members, sorted by name. A read that was empty already isn't
// the move's doing.
func (s *apiServer) emptyReads(moved, room string, members map[string]hub.Member) []string {
	empty := func(r engine.RoomRead) bool {
		if r.Room != room || !members[moved].OfTypes(r.Types) {
			return false
		}
		for _, m := range members {
			if m.Room == room && m.OfTypes(r.Types) {
				return false
			}
		}
		return true
	}
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	var out []string
	if sess := s.srv.session; sess != nil {
		for rule, reads := range sess.reads {
			if slices.ContainsFunc(reads, empty) {
				out = append(out, rule)
			}
		}
	}
	slices.Sort(out)
	return out
}
