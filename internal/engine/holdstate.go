package engine

import (
	"context"
	"encoding/json"
	"errors"
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

// HoldStates keeps HoldStates where they survive a restart.
type HoldStates interface {
	// Get returns the Rule's state, if one was saved.
	Get(key types.NamespacedName) (HoldState, bool)
	Put(ctx context.Context, key types.NamespacedName, s HoldState) error
	Delete(ctx context.Context, key types.NamespacedName) error
}

// statesRoot is where BusStates keeps hold states. 0003 reserved oikade/v1/_runtime, and no
// namespace starts with "_", so it can't collide with a plugin's root.
const statesRoot = "oikade/v1/_runtime/rules/"

// syncNamespace carries BusStates' sync marker. It's no namespace, for the same reason.
const syncNamespace = "_sync"

// syncTimeout bounds how long Sync waits for the broker.
const syncTimeout = 10 * time.Second

// BusStates keeps hold states as retained MQTT messages, one per Rule, at
// oikade/v1/_runtime/rules/<namespace>/<rule>.
type BusStates struct {
	Bus mqtt.Bus
	Log logr.Logger

	mu     sync.Mutex
	states map[types.NamespacedName]HoldState
	synced chan struct{}
	marker string
}

var _ HoldStates = (*BusStates)(nil)

// Sync subscribes to the saved states and returns once the broker has delivered them. Call it
// once, before the engine runs. The broker delivers retained messages before a message published
// after the subscription, so a marker published then arrives after every state.
func (b *BusStates) Sync(ctx context.Context) error {
	b.mu.Lock()
	b.states = map[types.NamespacedName]HoldState{}
	b.synced = make(chan struct{})
	b.marker = strconv.FormatInt(time.Now().UnixNano(), 36)
	b.mu.Unlock()
	if _, err := b.Bus.Subscribe(ctx, statesRoot+"+/+", b.received); err != nil {
		return fmt.Errorf("subscribe to hold states: %w", err)
	}
	if err := b.Bus.Publish(ctx, statesRoot+syncNamespace+"/"+b.marker, nil, false); err != nil {
		return fmt.Errorf("publish hold state marker: %w", err)
	}
	select {
	case <-b.synced:
		return nil
	case <-time.After(syncTimeout):
		return errors.New("timed out waiting for hold states")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *BusStates) received(m mqtt.Message) {
	ns, name, ok := strings.Cut(strings.TrimPrefix(m.Topic, statesRoot), "/")
	if !ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ns == syncNamespace {
		if name == b.marker {
			select {
			case <-b.synced:
			default:
				close(b.synced)
			}
		}
		return
	}
	key := types.NamespacedName{Namespace: ns, Name: name}
	if len(m.Payload) == 0 {
		delete(b.states, key)
		return
	}
	var s HoldState
	if err := json.Unmarshal(m.Payload, &s); err != nil {
		b.Log.Error(err, "Ignored unreadable hold state", "rule", key)
		return
	}
	b.states[key] = s
}

// Get implements HoldStates.
func (b *BusStates) Get(key types.NamespacedName) (HoldState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[key]
	return s, ok
}

// Put implements HoldStates.
func (b *BusStates) Put(ctx context.Context, key types.NamespacedName, s HoldState) error {
	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.states[key] = s
	b.mu.Unlock()
	return b.Bus.Publish(ctx, statesRoot+key.Namespace+"/"+key.Name, payload, true)
}

// Delete implements HoldStates. An empty retained message clears the saved state.
func (b *BusStates) Delete(ctx context.Context, key types.NamespacedName) error {
	b.mu.Lock()
	delete(b.states, key)
	b.mu.Unlock()
	return b.Bus.Publish(ctx, statesRoot+key.Namespace+"/"+key.Name, nil, true)
}

// memoryStates is a HoldStates that doesn't survive a restart, for an engine without one.
type memoryStates struct {
	mu     sync.Mutex
	states map[types.NamespacedName]HoldState
}

func (m *memoryStates) Get(key types.NamespacedName) (HoldState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[key]
	return s, ok
}

func (m *memoryStates) Put(_ context.Context, key types.NamespacedName, s HoldState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[key] = s
	return nil
}

func (m *memoryStates) Delete(_ context.Context, key types.NamespacedName) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, key)
	return nil
}
