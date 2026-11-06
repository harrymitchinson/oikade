package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// inventory keeps the live devices in step with the registry: the registry decides what exists and
// what it's called, and each driver instance sees its own devices there.
type inventory struct {
	registry *registry.Registry
	store    *hub.Store
	engine   *engine.Engine
	log      logr.Logger

	// mu orders changes, so that the hub takes them up in the registry's order.
	mu        sync.Mutex
	instances map[string]driver.InstanceKey
	// controls are arbitration settings by hardware, as a driver gave them, until they have a home
	// of their own (OIK-156).
	controls map[string][]homev1alpha1.CapabilityControl
}

func newInventory(reg *registry.Registry, store *hub.Store, eng *engine.Engine, log logr.Logger) *inventory {
	return &inventory{
		registry: reg, store: store, engine: eng, log: log,
		instances: map[string]driver.InstanceKey{}, controls: map[string][]homev1alpha1.CapabilityControl{},
	}
}

// instanceName is how the registry records a driver instance.
func instanceName(instance driver.InstanceKey) string {
	if instance.Namespace == "" {
		return instance.Driver + "/" + instance.Name
	}
	return instance.Driver + "/" + instance.Namespace + "/" + instance.Name
}

func hardwareKey(driverName, id string) string { return driverName + "\x00" + id }

// attach adds a driver instance, whose devices on record are known from now on, reported or not.
func (i *inventory) attach(instance driver.InstanceKey) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.instances[instanceName(instance)] = instance
	i.sync()
}

// paired records hardware a driver reports. It's in the registry and usable when this returns.
func (i *inventory) paired(instance driver.InstanceKey, app driver.AppDevice,
	control []homev1alpha1.CapabilityControl,
) (types.NamespacedName, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	d, changed, err := i.registry.Report(registry.Report{
		ID: app.ID, Driver: instanceName(instance), Name: app.Name, Room: app.Room, Capabilities: app.Capabilities,
	})
	if err != nil {
		return types.NamespacedName{}, err
	}
	hw := hardwareKey(d.Driver, d.ID)
	if len(control) > 0 && !equality.Semantic.DeepEqual(i.controls[hw], control) {
		i.controls[hw], changed = control, true
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
	return hub.Meta{
		DisplayName: d.Name, Room: d.Room, Instance: instance, Control: i.controls[hardwareKey(d.Driver, d.ID)],
	}, true
}

// key finds a driver's device in the hub.
func (i *inventory) key(instance driver.InstanceKey, id string) (types.NamespacedName, bool) {
	d, ok := i.registry.ByID(instanceName(instance), id)
	return key(d.Name), ok
}

// sync gives each driver instance its devices. Call with mu held.
func (i *inventory) sync() {
	metas := map[string]map[types.NamespacedName]hub.Meta{}
	for name := range i.instances {
		metas[name] = map[types.NamespacedName]hub.Meta{}
	}
	for _, d := range i.registry.List().Devices {
		if meta, ok := i.meta(d); ok {
			metas[d.Driver][key(d.Name)] = meta
		}
	}
	for name, devices := range metas {
		i.store.SetDevices(i.instances[name], devices)
	}
}

// announce tells your process about a device that changed.
func (i *inventory) announce(dev types.NamespacedName) {
	if snap, ok := i.store.Get(dev); ok && snap.Discovered {
		i.store.Discovered(dev, snap.Info)
	}
}

// claimed says who holds a device: every claim, and whether one is at Safety.
func (i *inventory) claimed(name string) (holders []string, safety bool) {
	for _, c := range i.engine.ControlOf(key(name)) {
		for _, o := range c.Claims {
			if o.Reverted {
				continue
			}
			holders = append(holders, o.Source)
			safety = safety || o.Level == homev1alpha1.ControlLevelSafety
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

func (i *inventory) move(name, room string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.registry.Move(name, room); err != nil {
		return err
	}
	i.sync()
	i.announce(key(name))
	i.log.Info("Moved device", "device", name, "room", room)
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
	moves := map[types.NamespacedName]hub.Move{}
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
	if holders, safety := i.claimed(name); safety && !allowSafety {
		return api.Response{
			Error:            fmt.Sprintf("%s holds %s at Safety", strings.Join(holders, ", "), name),
			NeedsAllowSafety: true,
		}
	}
	if err := i.registry.Remove(name); err != nil {
		return api.Response{Error: err.Error()}
	}
	i.sync()
	if err := i.engine.ForgetDevice(ctx, key(name)); err != nil {
		return api.Response{Error: fmt.Sprintf("removed %s, but couldn't end its claims: %v", name, err)}
	}
	i.log.Info("Removed device", "device", name)
	return api.Response{}
}

// replace moves name onto with's hardware. name's claims stay, since they're on the name, and the
// live values and arbitration settings go with it.
func (i *inventory) replace(name, with string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.refuseClaimed(with, "moved"); err != nil {
		return err
	}
	old, _ := i.registry.Get(name)
	source, _ := i.registry.Get(with)
	if err := i.registry.Replace(name, with); err != nil {
		return err
	}
	oldHardware := hardwareKey(old.Driver, old.ID)
	if control, ok := i.controls[oldHardware]; ok {
		i.controls[hardwareKey(source.Driver, source.ID)] = control
		delete(i.controls, oldHardware)
	}
	i.moveLive(map[string]string{with: name})
	i.announce(key(name))
	i.log.Info("Moved device name onto new hardware", "device", name, "from", with)
	return nil
}

// restore refuses a copy that would take a claimed name from its hardware, since claims stay with a
// name. After a real loss nothing is claimed, because claims are in the same store.
func (i *inventory) restore(from registry.Snapshot) ([]string, error) {
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
	moved, err := i.registry.Restore(from)
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

// device answers the commands that read and change the registry.
func (s *apiServer) device(req api.Request) api.Response {
	var err error
	switch req.Type {
	case api.TypeDevices:
		list := s.devices.registry.List()
		return api.Response{Registry: &list}
	case api.TypeDeviceName:
		err = s.devices.rename(req.Device, req.To)
	case api.TypeDeviceRoom:
		err = s.devices.move(req.Device, req.Room)
	case api.TypeDeviceReserve:
		err = s.devices.reserve(req.Device, req.Room)
	case api.TypeDeviceRemove:
		return s.devices.remove(s.ctx, req.Device, req.AllowSafety)
	case api.TypeDeviceReplace:
		err = s.devices.replace(req.Device, req.To)
	case api.TypeDeviceRestore:
		if req.Registry == nil {
			return api.Response{Error: "a restore needs a registry"}
		}
		moved, err := s.devices.restore(*req.Registry)
		if err != nil {
			return api.Response{Error: err.Error()}
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
