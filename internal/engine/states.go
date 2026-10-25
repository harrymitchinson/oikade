package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// HoldState is what a hold needs across a runtime restart, beyond what the clock and device
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
}

// HoldTarget is a hold's device or room, capability and value.
type HoldTarget struct {
	Device     string `json:"device,omitempty"`
	Room       string `json:"room,omitempty"`
	Capability string `json:"capability"`
	Value      any    `json:"value"`
}

// States keeps state, by Rule or Device, where it survives a restart.
type States[T any] interface {
	// Get returns the saved state, if there is one.
	Get(key types.NamespacedName) (T, bool)
	Put(ctx context.Context, key types.NamespacedName, s T) error
	Delete(ctx context.Context, key types.NamespacedName) error
}

// HoldStates keeps HoldStates, by Rule.
type HoldStates = States[HoldState]

// Roots of the runtime's retained state. The schedules design (OIK-5) reserved oikade/v1/_runtime,
// and no namespace starts with "_", so they can't collide with a plugin's root.
const (
	// HoldStatesRoot keeps a HoldState per Rule, at <root><namespace>/<rule>.
	HoldStatesRoot = "oikade/v1/_runtime/rules/"
	// ControlStatesRoot keeps a ControlState per Device, at <root><namespace>/<device>.
	ControlStatesRoot = "oikade/v1/_runtime/control/"
)

// syncNamespace carries Retained's sync marker. It's no namespace, for the same reason.
const syncNamespace = "_sync"

// syncRetry is how long Sync waits for its marker before publishing another.
const syncRetry = 10 * time.Second

// Retained keeps states as retained MQTT messages under Root, one per Rule or Device, at
// <Root><namespace>/<name>.
type Retained[T any] struct {
	Bus  mqtt.Bus
	Root string
	Log  logr.Logger

	mu     sync.Mutex
	states map[types.NamespacedName]T
	synced chan struct{}
	marker string
}

var _ HoldStates = (*Retained[HoldState])(nil)

// Sync subscribes to the saved states and returns once the broker has delivered them, or ctx
// ends. Call it once, before the engine runs. The broker delivers retained messages before a
// message published after the subscription, so a marker published then arrives after every
// state. Messages after the marker are the engine's own saves, which Put has already recorded.
func (b *Retained[T]) Sync(ctx context.Context) error {
	b.mu.Lock()
	b.states = map[types.NamespacedName]T{}
	b.synced = make(chan struct{})
	b.marker = strconv.FormatInt(time.Now().UnixNano(), 36)
	b.mu.Unlock()
	unsubscribe, err := b.Bus.Subscribe(ctx, b.Root+"+/+", b.received)
	if err != nil {
		return fmt.Errorf("subscribe to %s: %w", b.Root, err)
	}
	defer unsubscribe()
	for {
		if err := b.Bus.Publish(ctx, b.Root+syncNamespace+"/"+b.marker, nil, false); err != nil && ctx.Err() == nil {
			b.Log.Error(err, "Failed to publish state marker", "root", b.Root)
		}
		select {
		case <-b.synced:
			return nil
		case <-time.After(syncRetry):
			b.Log.Info("Still waiting for saved states from the broker", "root", b.Root)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *Retained[T]) received(m mqtt.Message) {
	ns, name, ok := strings.Cut(strings.TrimPrefix(m.Topic, b.Root), "/")
	if !ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.synced:
		return
	default:
	}
	if ns == syncNamespace {
		if name == b.marker {
			close(b.synced)
		}
		return
	}
	key := types.NamespacedName{Namespace: ns, Name: name}
	if len(m.Payload) == 0 {
		delete(b.states, key)
		return
	}
	var s T
	if err := json.Unmarshal(m.Payload, &s); err != nil {
		b.Log.Error(err, "Ignored unreadable saved state", "topic", m.Topic)
		return
	}
	b.states[key] = s
}

// Get implements States.
func (b *Retained[T]) Get(key types.NamespacedName) (T, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[key]
	return s, ok
}

// Put implements States.
func (b *Retained[T]) Put(ctx context.Context, key types.NamespacedName, s T) error {
	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.states[key] = s
	b.mu.Unlock()
	return b.Bus.Publish(ctx, b.Root+key.Namespace+"/"+key.Name, payload, true)
}

// Delete implements States. An empty retained message clears the saved state.
func (b *Retained[T]) Delete(ctx context.Context, key types.NamespacedName) error {
	b.mu.Lock()
	delete(b.states, key)
	b.mu.Unlock()
	return b.Bus.Publish(ctx, b.Root+key.Namespace+"/"+key.Name, nil, true)
}

// memoryStates is States that doesn't survive a restart, for an engine without one.
type memoryStates[T any] struct {
	mu     sync.Mutex
	states map[types.NamespacedName]T
}

func newMemoryStates[T any]() *memoryStates[T] {
	return &memoryStates[T]{states: map[types.NamespacedName]T{}}
}

func (m *memoryStates[T]) Get(key types.NamespacedName) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[key]
	return s, ok
}

func (m *memoryStates[T]) Put(_ context.Context, key types.NamespacedName, s T) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[key] = s
	return nil
}

func (m *memoryStates[T]) Delete(_ context.Context, key types.NamespacedName) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, key)
	return nil
}
