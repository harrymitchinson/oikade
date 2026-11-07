package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/store"
)

// HoldState is what a hold needs across a restart, beyond what the clock and device
// state recompute.
type HoldState struct {
	// Active is true once the hold's start has been written, and false once its end has.
	Active bool `json:"active"`
	// Latched is true from when a latched hold starts until a release.
	Latched bool `json:"latched,omitempty"`
	// OnAt is when an onDelay ends, and OffAt when an offDelay ends. Zero means none is running.
	OnAt  time.Time `json:"onAt,omitzero"`
	OffAt time.Time `json:"offAt,omitzero"`
	// Target is what the hold writes, so an edit that changes it can end the old one.
	Target HoldTarget `json:"target"`
	// Budget is a budget hold's plan and progress as JSON.
	Budget string `json:"budget,omitempty"`
}

// HoldTarget is a hold's device or room, capability and value.
type HoldTarget struct {
	Device string `json:"device,omitempty"`
	Room   string `json:"room,omitempty"`
	// DeviceTypes limits a room to devices of these types.
	DeviceTypes []devicetype.Type `json:"deviceTypes,omitempty"`
	Capability  string            `json:"capability"`
	Value       any               `json:"value"`
}

// States keeps state, by Rule or Device, where it survives a restart.
type States[T any] interface {
	// Get returns the saved state, if there is one.
	Get(key ident.ID) (T, bool)
	Put(ctx context.Context, key ident.ID, s T) error
	Delete(ctx context.Context, key ident.ID) error
}

// HoldStates keeps HoldStates, by Rule.
type HoldStates = States[HoldState]

// Namespaces of the store's saved state.
const (
	// HoldStatesNamespace keeps a HoldState per Rule.
	HoldStatesNamespace = "holds"
	// ControlStatesNamespace keeps a ControlState per Device.
	ControlStatesNamespace = "controls"
	// FireStatesNamespace keeps, per rule with timers, when they were last accounted for.
	FireStatesNamespace = "fires"
)

// Stored keeps states in a namespace of the embedded store, one per Rule or Device, at
// <namespace>/<name>. Put and Delete return once the change is on disk.
type Stored[T any] struct {
	Values *store.Namespace
	Log    *slog.Logger
}

var _ HoldStates = (*Stored[HoldState])(nil)

// Get implements States. A state it can't decode counts as none, as with Retained. The store fails
// a read only once it's closed, when Put fails too, so that can't overwrite a state.
func (s *Stored[T]) Get(key ident.ID) (T, bool) {
	state, ok, err := s.read(key)
	if err != nil {
		logging.OrDiscard(s.Log).Error("Ignored unreadable saved state", "err", err, "key", key.String())
		var none T
		return none, false
	}
	return state, ok
}

// Unreadable implements unreadableStates.
func (s *Stored[T]) Unreadable(key ident.ID) error {
	_, _, err := s.read(key)
	return err
}

func (s *Stored[T]) read(key ident.ID) (T, bool, error) {
	var state T
	payload, ok, err := s.Values.Get(key.String())
	if err != nil || !ok {
		return state, false, err
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		return state, false, err
	}
	return state, true, nil
}

// Names returns the names of the states saved in a namespace.
func (s *Stored[T]) Names(namespace string) ([]string, error) {
	keys, err := s.Values.Keys()
	if err != nil {
		return nil, err
	}
	prefix := ident.ID{Namespace: namespace}.String()
	var names []string
	for _, k := range keys {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			names = append(names, name)
		}
	}
	return names, nil
}

// Put implements States.
//
// It ignores ctx: the write is local, and a hold that has just written its device must save that
// even during shutdown, or it would repeat the write after the restart.
func (s *Stored[T]) Put(_ context.Context, key ident.ID, state T) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.Values.Put(key.String(), payload)
}

// Delete implements States. It ignores ctx, as Put does.
func (s *Stored[T]) Delete(_ context.Context, key ident.ID) error {
	return s.Values.Delete(key.String())
}

// memoryStates is States that doesn't survive a restart, for an engine without one.
type memoryStates[T any] struct {
	mu     sync.Mutex
	states map[ident.ID]T
}

func newMemoryStates[T any]() *memoryStates[T] {
	return &memoryStates[T]{states: map[ident.ID]T{}}
}

func (m *memoryStates[T]) Get(key ident.ID) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[key]
	return s, ok
}

func (m *memoryStates[T]) Put(_ context.Context, key ident.ID, s T) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[key] = s
	return nil
}

func (m *memoryStates[T]) Delete(_ context.Context, key ident.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, key)
	return nil
}
