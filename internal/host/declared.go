package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/registry"
)

// declaredNamespace keeps the device declarations of the home last attached, saved before they're
// applied, so a host that crashed in between applies them when it starts.
const (
	declaredNamespace = "home-inventory"
	declaredKey       = "declared"
)

// saved is what declaredKey keeps: the home's declarations, and the keys of the cases and averages
// its version has, which are the ones a rename may move state to.
type saved struct {
	Inventory []wire.DeviceDeclaration `json:"inventory"`
	Keys      []string                 `json:"keys,omitempty"`
}

// loadDeclared applies the declarations the last process to attach saved. Applying them again
// changes nothing, unless a crash stopped the host before it had.
func (i *inventory) loadDeclared() error {
	raw, ok, err := i.declarations.Get(declaredKey)
	if err != nil || !ok {
		return err
	}
	var last saved
	if err := json.Unmarshal(raw, &last); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	plan, err := i.registry.Plan(wire.Declarations(last.Inventory))
	if err != nil {
		return err
	}
	return i.applyDeclared(last.Inventory, last.Keys, plan)
}

// planDeclared says what a version's declarations would do, refusing what can't be.
func (i *inventory) planDeclared(decls []wire.DeviceDeclaration) (registry.Declared, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planLocked(decls)
}

// planLocked refuses declarations the registry can't take. A renamed device's claims go with it,
// but hardware a declared name takes over keeps none, as with replace, and a reserved name that goes
// would leave its claims behind, so either is refused while something holds it. Call with mu held.
func (i *inventory) planLocked(decls []wire.DeviceDeclaration) (registry.Declared, error) {
	plan, err := i.registry.Plan(wire.Declarations(decls))
	if err != nil {
		return plan, err
	}
	for _, from := range slices.Sorted(maps.Keys(plan.Absorbed)) {
		if holders, _ := i.claimed(from); len(holders) > 0 {
			return plan, fmt.Errorf("%s holds %s, so %s can't take its hardware: claims stay with a name, so deploy a "+
				"version that doesn't claim %s first", strings.Join(holders, ", "), from, plan.Absorbed[from], from)
		}
	}
	for from := range plan.Renamed {
		if _, unreadable := i.engine.UnreadableClaim(key(from)); unreadable {
			return plan, fmt.Errorf("the claims on %s can't be read, so it can't be renamed", from)
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
// applies them. keys are its version's own cases and averages (wire.OwnCases), the only keys a
// rename moves state to: a group's case can be keyed by a name a device has too.
func (i *inventory) takeDeclared(decls []wire.DeviceDeclaration, keys []string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	plan, err := i.planLocked(decls)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(saved{Inventory: decls, Keys: keys})
	if err != nil {
		return err
	}
	if err := i.declarations.Put(declaredKey, raw); err != nil {
		return err
	}
	return i.applyDeclared(decls, keys, plan)
}

// applyDeclared makes the registry what plan says, and the hub follow it. A renamed device's claims
// and its cases' state move first, so that they're never on neither name, and go back if the
// registry can't take the new names. Call with mu held.
func (i *inventory) applyDeclared(decls []wire.DeviceDeclaration, keys []string, plan registry.Declared) error {
	if len(plan.Renamed) > 0 {
		done, err := i.moveState(plan.Renamed, keys)
		if err != nil {
			return err
		}
		defer done()
	}
	done, err := i.registry.Declare(wire.Declarations(decls))
	if err != nil {
		// The registry is as it was, so its names get their state back.
		i.moveBack(plan.Renamed, keys)
		return err
	}
	if len(done.Fresh) > 0 {
		// Their old hardware's values aren't theirs: drop them, and take them up afresh.
		i.syncExcept(done.Fresh)
	}
	live := maps.Clone(done.Renamed)
	maps.Copy(live, done.Absorbed)
	i.moveLive(live)
	for _, name := range done.Touched {
		i.announce(key(name))
	}
	for _, c := range done.Changes {
		i.log.Info("Applied device declaration", "change", c)
	}
	return nil
}

// moveState moves renamed devices' claims, from old names to new, and the state of their cases and
// averages, to the new keys in keys, or with none given, any. done ends the move, once the registry
// has renamed them. A failure puts back what moved. Call with mu held.
func (i *inventory) moveState(renamed map[string]string, keys []string) (done func(), err error) {
	rename := func(k string) string {
		if to := control.RenamedKey(k, renamed); keys == nil || slices.Contains(keys, to) {
			return to
		}
		return k
	}
	done, err = i.engine.MoveDevices(context.Background(), deviceMoves(renamed), ruleSources(rename))
	if err != nil {
		done() // It put back what it had moved.
		return func() {}, err
	}
	if i.renamed != nil {
		if err := i.renamed(rename); err != nil {
			// Before done, so the old names never get arbiters without their claims.
			i.moveBack(renamed, keys)
			done()
			return func() {}, err
		}
	}
	return done, nil
}

// moveBack undoes moveState's renamed and keys, for names the registry kept. Call with mu held.
func (i *inventory) moveBack(renamed map[string]string, keys []string) {
	back := invert(renamed)
	unrename := func(k string) string {
		if keys != nil && !slices.Contains(keys, k) {
			return k
		}
		return control.RenamedKey(k, back)
	}
	done, err := i.engine.MoveDevices(context.Background(), deviceMoves(back), ruleSources(unrename))
	done()
	if err == nil && i.renamed != nil {
		err = i.renamed(unrename)
	}
	if err != nil {
		i.log.Error("Failed to move claims back to the names the registry has", "err", err)
	}
}

func deviceMoves(renamed map[string]string) map[ident.ID]ident.ID {
	moves := make(map[ident.ID]ident.ID, len(renamed))
	for from, to := range renamed {
		moves[key(from)] = key(to)
	}
	return moves
}

// ruleSources renames claims' sources that are cases, by rename.
func ruleSources(rename func(string) string) func(string) string {
	return func(source string) string {
		if k, ok := strings.CutPrefix(source, "Rule/"); ok {
			return "Rule/" + rename(k)
		}
		return source
	}
}

func invert[K comparable](m map[K]K) map[K]K {
	out := make(map[K]K, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// declared says the home's code declares a device, so commands can't change it.
func (i *inventory) declared(name string) error {
	if d, ok := i.registry.Get(name); ok && d.Declared {
		return fmt.Errorf("%s is declared in your home's code: change it there, and deploy", name)
	}
	return nil
}

// renameRules moves what's on record for cases and averages to the keys rename gives them: holds,
// fires, pauses and averages. Every new key is written before any old one goes, so keys can swap.
func (s *server) renameRules(rename func(string) string) error {
	return errors.Join(
		renameKeys(s.holds, rename), renameKeys(s.fires, rename), renameKeys(s.paused, rename),
		renameKeys(s.averages, rename),
	)
}

func renameKeys[T any](values *engine.Stored[T], rename func(string) string) error {
	if values == nil {
		return nil
	}
	names, err := values.Names()
	if err != nil {
		return err
	}
	ctx := context.Background()
	moves, values2 := map[string]string{}, map[string]T{}
	for _, name := range names {
		if to := rename(name); to != name {
			if v, ok := values.Get(ident.ID{Name: name}); ok {
				moves[name], values2[name] = to, v
			}
		}
	}
	for from, to := range moves {
		if err := values.Put(ctx, ident.ID{Name: to}, values2[from]); err != nil {
			return err
		}
	}
	taken := invert(moves)
	for from := range moves {
		if _, ok := taken[from]; ok {
			continue // Another key's value is here now.
		}
		if err := values.Delete(ctx, ident.ID{Name: from}); err != nil {
			return err
		}
	}
	return nil
}
