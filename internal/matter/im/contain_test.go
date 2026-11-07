package im

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// A command that panics does so with the Node's lock held: the lock must come free, or every other
// controller would wait on it for ever.
func TestAPanicUnderTheNodesLockLeavesItFree(t *testing.T) {
	node, c := onOffNode()
	c.Commands = []Command{{ID: 0x01, Invoke: func(*Request, *tlv.Reader) (*Response, error) {
		panic("a bug in a command")
	}}}
	bad, _ := pair(t, node)
	short, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := bad.Invoke(short, CommandPath{Endpoint: 1, Cluster: 6, Command: 0x01}, nil, false, 0); err == nil {
		t.Error("a command that panicked was answered")
	}

	good, _ := pair(t, node)
	ctx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	reports, err := good.Read(ctx, AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0})
	if err != nil || len(reports) != 1 {
		t.Fatalf("reading after the panic: %+v, %v", reports, err)
	}
	updated := make(chan struct{})
	go func() {
		node.Update(func() {})
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("the Node's lock is still held")
	}
}

// observed is an Observer that hears only panics.
type observed struct{ panics atomic.Int32 }

func (*observed) Wrote(uint32, Status)   {}
func (*observed) Invoked(uint32, Status) {}
func (*observed) Reported()              {}
func (o *observed) Panicked(part string) {
	if part == contain.Subscription {
		o.panics.Add(1)
	}
}

// A subscription that panics ends with its session, so its controller starts again rather than wait
// for reports that won't come.
func TestASubscriptionThatPanicsEndsItsSession(t *testing.T) {
	var broken atomic.Bool
	c := &Cluster{ID: 6, Revision: 1, Attributes: []Attribute{{ID: 0, Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error {
		if broken.Load() {
			panic("a bug in a read")
		}
		w.PutBool(tag, true)
		return nil
	}}}}
	node := &Node{}
	_ = node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}})
	o := &observed{}
	var ended atomic.Int32
	srv := &Server{Node: node, Observer: o, Log: logging.Test(t), EndSession: func(*transport.Session) { ended.Add(1) }}
	client, _, _, _ := pairWith(t, srv, transport.SecureSessionConfig{Auth: transport.AuthPASE})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, _, err := client.Subscribe(ctx, 0, 60, AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0}); err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	node.Update(func() { c.Changed(0) })
	waitFor(t, "the subscription to end", func() bool { return srv.Subscriptions() == 0 })
	if o.panics.Load() != 1 || ended.Load() != 1 {
		t.Errorf("%d panics heard, %d sessions ended: want 1 of each", o.panics.Load(), ended.Load())
	}
}
