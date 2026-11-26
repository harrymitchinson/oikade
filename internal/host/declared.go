package host

import (
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"oikade.dev/internal/control"
	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/registry"
)

// declaredNamespace keeps the device declarations of the home last attached, saved before they're
// applied, so a host that crashed in between applies them when it starts.
const (
	declaredNamespace = "home-inventory"
	declaredKey       = "declared"
)

// saved is what declaredKey keeps: the home's declarations.
type saved struct {
	Inventory []wire.DeviceDeclaration `json:"inventory"`
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
	return i.applyDeclared(last.Inventory)
}

// planDeclared says what a version's declarations would do, refusing what can't be.
func (i *inventory) planDeclared(decls []wire.DeviceDeclaration) (registry.Declared, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planLocked(decls)
}

// planLocked refuses declarations the registry can't take. What the host keeps for a device is by
// its hardware, so a rename or a declared name taking over hardware moves nothing, but a reserved
// name that goes, or takes hardware, would leave its claims behind, so that's refused while
// something holds it. Call with mu held.
func (i *inventory) planLocked(decls []wire.DeviceDeclaration) (registry.Declared, error) {
	plan, err := i.registry.Plan(wire.Declarations(decls))
	if err != nil {
		return plan, err
	}
	for _, name := range plan.Removed {
		if holders, _ := i.claimed(ident.ID{Name: registry.KeyOf("", "", name)}); len(holders) > 0 {
			return plan, fmt.Errorf("%s holds %s, so the name can't stop being declared: deploy a version that doesn't "+
				"claim it first", strings.Join(holders, ", "), name)
		}
	}
	for _, d := range wire.Declarations(decls) {
		if was, ok := i.registry.Get(d.Name); !ok || d.ID == "" || was.ID != "" {
			continue
		}
		if holders, _ := i.claimed(ident.ID{Name: registry.KeyOf("", "", d.Name)}); len(holders) > 0 {
			return plan, fmt.Errorf("%s holds %s, which is reserved, so it can't take hardware: deploy a version that "+
				"doesn't claim it first", strings.Join(holders, ", "), d.Name)
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
	raw, err := json.Marshal(saved{Inventory: decls})
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
	i.sync()
	renamed := maps.Clone(done.Renamed)
	maps.Copy(renamed, done.Absorbed)
	for _, from := range slices.Sorted(maps.Keys(renamed)) {
		if d, ok := i.registry.Get(renamed[from]); ok {
			i.record(api.Event{Type: api.EventRename, Device: d.Key(), Previous: from, Message: d.Name})
		}
	}
	for _, k := range done.Touched {
		i.announce(ident.ID{Name: k})
	}
	for _, c := range done.Changes {
		i.log.Info("Applied device declaration", "change", c)
	}
	return nil
}

// leavingSafety names the Safety rules a device would drop out of under decls: a room write it's in
// now, as one of the device types it's limited to, and wouldn't be once its declared room or type
// is.
func (i *inventory) leavingSafety(decls []wire.DeviceDeclaration, old, next []wire.RuleInfo) []string {
	i.mu.Lock()
	before := i.registry.List()
	i.mu.Unlock()
	after, err := registry.Declaring(before, wire.Declarations(decls))
	if err != nil {
		return []string{"what the declarations take out of Safety rules can't be checked: " + err.Error()}
	}
	writes := func(rooms []wire.RoomOfTypes, d registry.Device) bool {
		return slices.ContainsFunc(rooms, func(room wire.RoomOfTypes) bool {
			return d.Room == room.Room && slices.Contains(d.Capabilities, room.Capability) &&
				!slices.Contains(room.Except, d.Key()) && devicetype.Any(d.DeviceTypes(), room.DeviceTypes)
		})
	}
	var out []string
	for b, a := range declaredPairs(before, after) {
		// A rule the next version drops is a finding of its own.
		for _, r := range next {
			was := slices.IndexFunc(old, func(o wire.RuleInfo) bool { return o.Name == r.Name && o.Safety })
			if r.Safety && was >= 0 && writes(old[was].Rooms, b) && !writes(r.Rooms, a) {
				out = append(out, fmt.Sprintf("%s would leave the devices %s writes at Safety, by its declared room or "+
					"type, so it would stop protecting it", a.Name, r.Name))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// declaring is what a version's declarations would change of the hub's devices: their members now
// and once decls are applied, and a line for each device a declared room or type puts in a room
// whose decision it won't take, as it has its own under next, the version's control. old, the
// running one's, says what a device retyped in its own room kept already.
func (i *inventory) declaring(decls []wire.DeviceDeclaration, old, next []wire.DeviceControl) (members, []string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	before := i.registry.List()
	after, err := registry.Declaring(before, wire.Declarations(decls))
	if err != nil {
		return members{}, nil, err
	}
	var kept []string
	for b, a := range declaredPairs(before, after) {
		if !i.onHub(a) || a.Room == b.Room && slices.Equal(a.DeviceTypes(), b.DeviceTypes()) {
			// The hub doesn't arbitrate it, or its room is the same one, whose decisions the home's
			// code shows beside its own.
			continue
		}
		caps := keptDecisions(next, a)
		if a.Room == b.Room {
			// Retyped in the same room: only what it didn't keep already is news.
			was := keptDecisions(old, b)
			caps = slices.DeleteFunc(caps, func(c string) bool { return slices.Contains(was, c) })
		}
		if len(caps) > 0 {
			refs := make([]string, len(caps))
			for k, c := range caps {
				refs[k] = ident.CapabilityRef(a.Name, c).String()
			}
			kept = append(kept, fmt.Sprintf("%s keeps its own decision rather than %s's, "+
				"unless the home hands it over with Then", strings.Join(refs, " and "), a.Room))
		}
	}
	slices.Sort(kept)
	return members{before: i.membersOf(before), after: i.membersOf(after)}, kept, nil
}

// membersOf are the devices of snap the hub would have, by key, as sync puts them there. Call with
// mu held.
func (i *inventory) membersOf(snap registry.Snapshot) map[string]hub.Member {
	out := map[string]hub.Member{}
	for _, d := range snap.Devices {
		if i.onHub(d) {
			out[d.Key()] = hub.Member{Room: d.Room, DeviceTypes: d.DeviceTypes()}
		}
	}
	return out
}

// keptDecisions are the capabilities a device has its own decision on that its room also decides,
// sorted, which it keeps rather than taking the room's. One whose decision hands over to the room's
// with Then has every line of the room's in its own, below them, and isn't one. A room's decision
// with no lines but the override line can't tell, and isn't one either.
func keptDecisions(settings []wire.DeviceControl, d registry.Device) []string {
	var kept []string
	for _, c := range settings {
		if c.Room == "" || c.Room != d.Room || !slices.Contains(d.Capabilities, c.Entry.Capability) ||
			len(c.DeviceTypes) > 0 && !devicetype.Any(d.DeviceTypes(), c.DeviceTypes) {
			continue
		}
		if slices.ContainsFunc(settings, func(o wire.DeviceControl) bool {
			return o.Device == d.Key() && o.Entry.Capability == c.Entry.Capability &&
				slices.ContainsFunc(c.Entry.Lines, func(l string) bool {
					return l != control.LineOverride && !slices.Contains(o.Entry.Lines, l)
				})
		}) {
			kept = append(kept, c.Entry.Capability)
		}
	}
	slices.Sort(kept)
	return slices.Compact(kept)
}

// declaredPairs are the devices of after that were on record in before, each with what it was there:
// the same endpoint, before a declaration renamed, moved or retyped it.
func declaredPairs(before, after registry.Snapshot) iter.Seq2[registry.Device, registry.Device] {
	return func(yield func(registry.Device, registry.Device) bool) {
		was := make(map[uint16]registry.Device, len(before.Devices))
		for _, b := range before.Devices {
			if !b.Removed {
				was[b.Endpoint] = b
			}
		}
		for _, a := range after.Devices {
			if b, ok := was[a.Endpoint]; ok && !a.Removed && !yield(b, a) {
				return
			}
		}
	}
}
