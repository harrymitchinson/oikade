package host

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// declaredNamespace keeps the device declarations of the home last attached, saved before they're
// applied, so a host that crashed in between applies them when it starts.
const (
	declaredNamespace = "home-inventory"
	declaredKey       = "declared"
)

// loadDeclared applies the declarations the last process to attach saved. Applying them again
// changes nothing, unless a crash stopped the host before it had.
func (i *inventory) loadDeclared() error {
	raw, ok, err := i.declarations.Get(declaredKey)
	if err != nil || !ok {
		return err
	}
	var decls []wire.DeviceDeclaration
	if err := json.Unmarshal(raw, &decls); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.applyDeclared(decls)
}

// planDeclared says what a version's declarations would do, refusing what can't be.
func (i *inventory) planDeclared(decls []wire.DeviceDeclaration) (registry.Declared, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planLocked(decls)
}

// planLocked refuses declarations the registry can't take, and a rename or removal of a device
// something holds: claims stay with a name, and would be left behind. Call with mu held.
func (i *inventory) planLocked(decls []wire.DeviceDeclaration) (registry.Declared, error) {
	plan, err := i.registry.Plan(wire.Declarations(decls))
	if err != nil {
		return plan, err
	}
	for _, from := range slices.Sorted(maps.Keys(plan.Renamed)) {
		if holders, _ := i.claimed(from); len(holders) > 0 {
			return plan, fmt.Errorf("%s holds %s, and its claims don't follow a rename yet, so %s can't become %s: "+
				"deploy a version that doesn't claim it first", strings.Join(holders, ", "), from, from, plan.Renamed[from])
		}
	}
	for _, name := range plan.Removed {
		if holders, _ := i.claimed(name); len(holders) > 0 {
			return plan, fmt.Errorf("%s holds %s, so the name can't stop being declared: deploy a version that doesn't "+
				"claim it first", strings.Join(holders, ", "), name)
		}
	}
	return plan, nil
}

// takeDeclared takes up the declarations of a process that's connecting: it saves them, then
// applies them.
func (i *inventory) takeDeclared(decls []wire.DeviceDeclaration) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, err := i.planLocked(decls); err != nil {
		return err
	}
	raw, err := json.Marshal(decls)
	if err != nil {
		return err
	}
	if err := i.declarations.Put(declaredKey, raw); err != nil {
		return err
	}
	return i.applyDeclared(decls)
}

// applyDeclared makes the registry what decls say, and the hub follow it. Call with mu held.
func (i *inventory) applyDeclared(decls []wire.DeviceDeclaration) error {
	done, err := i.registry.Declare(wire.Declarations(decls))
	if err != nil {
		return err
	}
	if len(done.Fresh) > 0 {
		// Their old hardware's values aren't theirs: drop them, and take them up afresh.
		i.syncExcept(done.Fresh)
	}
	i.moveLive(done.Renamed)
	for _, name := range done.Touched {
		i.announce(key(name))
	}
	for _, c := range done.Changes {
		i.log.Info("Applied device declaration", "change", c)
	}
	return nil
}

// declared says the home's code declares a device, so commands can't change it.
func (i *inventory) declared(name string) error {
	if d, ok := i.registry.Get(name); ok && d.Declared {
		return fmt.Errorf("%s is declared in your home's code: change it there, and deploy", name)
	}
	return nil
}
