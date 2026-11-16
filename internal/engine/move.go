package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/harrymitchinson/oikade/internal/ident"
)

// MoveDevices takes devices' claims and arbitration state to their new names, from moves' keys to
// its values, so a rename doesn't end what holds a device. source renames each claim's source, as a
// case keyed by the old name is keyed by the new one. Call it before the store renames the devices,
// then done once it has: until then, the old names get no arbiters, so nothing decides them afresh
// and writes them in between. A device whose saved claims can't be read isn't moved, and nor is
// anything else.
func (e *Engine) MoveDevices(ctx context.Context, moves map[ident.ID]ident.ID, source func(string) string) (done func(), err error) {
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	for from := range moves {
		if err := e.unreadableClaims(from); err != nil {
			return func() {}, fmt.Errorf("the claims on %s can't be read, so it can't be renamed: %w", from.Name, err)
		}
	}
	e.mu.Lock()
	states := map[ident.ID]ControlState{}
	for from := range moves {
		e.moving[from] = true
		if len(e.arbs[from]) == 0 {
			continue
		}
		// The live state is newer than any saved.
		st := ControlState{}
		for name, ar := range e.arbs[from] {
			st[name] = ar.a.State()
		}
		states[from] = st
		for name := range st {
			e.dropArb(from, name, false)
		}
		delete(e.saveDue, from)
	}
	e.mu.Unlock()
	done = func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		// Both: the old names are due too, in case the store didn't rename them after all.
		for from, to := range moves {
			delete(e.moving, from)
			e.controlDue[from], e.controlDue[to] = true, true
		}
		e.signal()
	}
	// Every state is read before any is written, so names can swap.
	for from := range moves {
		if _, ok := states[from]; !ok {
			if st, ok := e.Controls.Get(from); ok {
				states[from] = st
			}
		}
	}
	// The new names first, so a crash in between leaves the claims under both, not neither. A
	// failure puts back what was, live state included, which nothing else has saved.
	var written []ident.ID
	undo := func(cause error) error {
		for _, to := range written {
			if _, own := states[to]; !own {
				_ = e.Controls.Delete(ctx, to)
				delete(e.savedControls, to)
			}
		}
		for from, st := range states {
			if err := e.Controls.Put(ctx, from, st); err != nil {
				e.Log.Error("Failed to put claims back after a rename failed", "err", err, "device", from)
			}
		}
		return cause
	}
	for from, st := range states {
		to := moves[from]
		renamed := ControlState{}
		for name, s := range st {
			s.Claims = slices.Clone(s.Claims)
			for i := range s.Claims {
				s.Claims[i].Source = source(s.Claims[i].Source)
			}
			renamed[name] = s
		}
		if err := e.Controls.Put(ctx, to, renamed); err != nil {
			return done, undo(fmt.Errorf("moving the claims on %s to %s: %w", from.Name, to.Name, err))
		}
		written = append(written, to)
		raw, _ := json.Marshal(renamed)
		e.savedControls[to] = string(raw)
	}
	for from := range states {
		if _, taken := states[reverse(moves, from)]; taken {
			continue // Another device's claims are there now.
		}
		if err := e.Controls.Delete(ctx, from); err != nil {
			return done, undo(fmt.Errorf("moving the claims off %s: %w", from.Name, err))
		}
		delete(e.savedControls, from)
	}
	return done, nil
}

// reverse finds the device moving to name, if one is.
func reverse(moves map[ident.ID]ident.ID, name ident.ID) ident.ID {
	for from, to := range moves {
		if to == name {
			return from
		}
	}
	return ident.ID{}
}
