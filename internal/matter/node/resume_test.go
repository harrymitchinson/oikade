package node

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// memSubscriptions is an im.SubscriptionStore in memory, which outlives the nodes that use it.
type memSubscriptions struct {
	mu   sync.Mutex
	subs map[uint32]im.SavedSubscription
}

func (m *memSubscriptions) SaveSubscription(s im.SavedSubscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subs == nil {
		m.subs = map[uint32]im.SavedSubscription{}
	}
	m.subs[s.ID] = s
	return nil
}

func (m *memSubscriptions) DeleteSubscription(_ uint8, id uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subs, id)
	return nil
}

func (m *memSubscriptions) Subscriptions() ([]im.SavedSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]im.SavedSubscription, 0, len(m.subs))
	for _, s := range m.subs {
		out = append(out, s)
	}
	return out, nil
}

func (m *memSubscriptions) ids() []uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]uint32, 0, len(m.subs))
	for id := range m.subs {
		ids = append(ids, id)
	}
	return ids
}

// TestResumeAfterRestart: a controller subscribes, the node restarts on the same store, and the
// controller hears a report under its old subscription ID without subscribing again. Then a
// subscription that replaces it, and removing the fabric, each delete its record.
func TestResumeAfterRestart(t *testing.T) {
	dir, saved := t.TempDir(), &memSubscriptions{}
	n, fake, stop := start(t, Config{DataDir: dir, Subscriptions: saved})
	waitFor(t, "the initial window", func() bool { return fake.commissionable() != nil })
	cm := nodetest.New(t)
	cm.AcceptCASE()
	admin := cm.Commission(addr(n), testSetup.Passcode)

	label := im.AttributePath{Endpoint: 0, Cluster: clusters.IDBasicInformation, Attribute: 0x0005} // NodeLabel
	parts := im.AttributePath{Endpoint: 0, Cluster: 0x001D, Attribute: 0x0003}                      // the root's Descriptor PartsList
	_, id, _, reports, err := admin.Subscribe(nodetest.Context(t), 0, 60, label, parts)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the subscription to be saved", func() bool { return slices.Equal(saved.ids(), []uint32{id}) })

	port := n.cfg.Port
	stop()
	for len(reports) > 0 {
		<-reports
	}
	n, _, _ = start(t, Config{DataDir: dir, Port: port, Subscriptions: saved})
	select {
	case got := <-reports:
		if len(got) != 2 {
			t.Errorf("resumed priming report %+v, want NodeLabel and PartsList", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no report under the old subscription ID after the restart")
	}
	waitFor(t, "the resumed subscription to run", func() bool { return n.Subscriptions() == 1 })
	if !slices.Equal(saved.ids(), []uint32{id}) {
		t.Errorf("saved %v after resuming, want %v", saved.ids(), []uint32{id})
	}

	// A change reaches the resumed subscription.
	if _, err := n.Bridge().AddDevice(2, clusters.DeviceOnOffPlug, "Fan", "fan-1", clusters.NewOnOff().Cluster()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("no report of a change on the resumed subscription")
	}

	// A new subscription without keeping the others replaces it, and its record.
	admin = cm.CASE(addr(n))
	_, id2, _, _, err := admin.Subscribe(nodetest.Context(t), 0, 60, label)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "only the new subscription to be saved", func() bool { return slices.Equal(saved.ids(), []uint32{id2}) })

	// Removing the fabric deletes its records.
	if f := cm.Invoke(admin, clusters.IDOperationalCredentials, 0x0A, func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) }); f[0] != uint64(0) {
		t.Fatalf("RemoveFabric: %v", f)
	}
	waitFor(t, "the fabric's records to go", func() bool { return len(saved.ids()) == 0 })
}

// TestResumeGivesUpAfterOneMaxInterval: a subscriber that can't be reached is tried after 1, 2 and
// 4 s, and its record goes once the next wait would pass one max interval, 10 s here.
func TestResumeGivesUpAfterOneMaxInterval(t *testing.T) {
	saved := &memSubscriptions{}
	rec := im.SavedSubscription{FabricIndex: 1, ID: 42, PeerNode: 7, Max: 10}
	_ = saved.SaveSubscription(rec)
	srv := &im.Server{Node: &im.Node{}, Saved: saved, Log: logging.Test(t)}
	if _, err := srv.LoadSubscriptions(); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 11, 10, 22, 0, 0, 0, time.UTC))
	start := clk.Now()
	var tries []time.Duration
	var mu sync.Mutex
	r := &resumer{
		clock: clk, server: srv, end: func(*transport.Session) {}, log: logging.Test(t),
		dial: func(context.Context, im.SavedSubscription) (*transport.Session, error) {
			mu.Lock()
			defer mu.Unlock()
			tries = append(tries, clk.Now().Sub(start))
			return nil, errors.New("no route to host")
		},
	}
	var done atomic.Bool
	go func() { r.resume(context.Background(), rec); done.Store(true) }()
	for !done.Load() {
		if clk.HasWaiters() {
			if len(saved.ids()) != 1 {
				t.Fatal("record deleted while still trying")
			}
			clk.Step(time.Second)
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}; !slices.Equal(tries, want) {
		t.Errorf("tried at %v, want %v", tries, want)
	}
	if len(saved.ids()) != 0 {
		t.Error("record kept after giving up")
	}
}

// TestResumeStopsWithTheNode keeps the record of a subscription still being retried when the node
// stops, and forgets one the subscriber refused.
func TestResumeStopsWithTheNode(t *testing.T) {
	saved := &memSubscriptions{}
	kept, refused := im.SavedSubscription{FabricIndex: 1, ID: 1, Max: 60}, im.SavedSubscription{FabricIndex: 1, ID: 2, Max: 60}
	_ = saved.SaveSubscription(kept)
	_ = saved.SaveSubscription(refused)
	srv := &im.Server{Node: &im.Node{}, Saved: saved, Log: logging.Test(t)}
	if _, err := srv.LoadSubscriptions(); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now())
	r := &resumer{
		clock: clk, server: srv, end: func(*transport.Session) {}, log: logging.Test(t),
		dial: func(_ context.Context, s im.SavedSubscription) (*transport.Session, error) {
			if s.ID == refused.ID {
				return nil, im.ErrRefused
			}
			return nil, errors.New("no route to host")
		},
	}
	r.resume(context.Background(), refused)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.resume(ctx, kept); close(done) }()
	waitFor(t, "a retry to wait", clk.HasWaiters)
	cancel()
	<-done
	if !slices.Equal(saved.ids(), []uint32{kept.ID}) {
		t.Errorf("saved %v, want only %d", saved.ids(), kept.ID)
	}
}
