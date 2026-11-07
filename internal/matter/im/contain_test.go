package im

import (
	"context"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/contain"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// A command that panics does so with the Node's lock held: the lock must come free, or every other
// controller would wait on it for ever.
func TestAPanicUnderTheNodesLockLeavesItFree(t *testing.T) {
	node, c := onOffNode()
	c.Commands = []Command{{ID: 0x01, Invoke: func(*Request, *tlv.Reader) (*Response, error) {
		panic("a bug in a command")
	}}}
	before := contain.Panics(contain.Handler)
	bad, _ := pair(t, node)
	short, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := bad.Invoke(short, CommandPath{Endpoint: 1, Cluster: 6, Command: 0x01}, nil, false, 0); err == nil {
		t.Error("a command that panicked was answered")
	}
	if got := contain.Panics(contain.Handler); got != before+1 {
		t.Errorf("%d panics counted, want %d", got, before+1)
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
