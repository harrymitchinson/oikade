package im

import (
	"sync"
	"testing"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// readFiltered reads every attribute of endpoint 1's On/Off cluster, telling the server which
// version of each cluster it already has.
func readFiltered(t *testing.T, c *Client, filters func(w *tlv.Writer)) ([]AttributeReport, error) {
	t.Helper()
	ctx := timeoutCtx(t)
	ex, err := c.Session.NewExchange()
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	putPaths(&w, tlv.Context(0), []AttributePath{{Endpoint: 1, Cluster: 6, WildAttribute: true}})
	w.PutBool(tlv.Context(3), true)
	w.StartArray(tlv.Context(4)) // DataVersionFilters
	filters(&w)
	w.End()
	w.PutUint(tagRevision, revision)
	w.End()
	if err := ex.Send(ctx, imMessage(OpReadRequest, w.Bytes())); err != nil {
		t.Fatal(err)
	}
	reports, _, err := receiveReports(ctx, ex, nil)
	return reports, err
}

// filter writes a DataVersionFilterIB.
func filter(endpoint uint16, cluster, version uint32) func(w *tlv.Writer) {
	return func(w *tlv.Writer) {
		w.StartStructure(tlv.Anonymous)
		w.StartList(tlv.Context(0)) // ClusterPathIB
		w.PutUint(tlv.Context(1), uint64(endpoint))
		w.PutUint(tlv.Context(2), uint64(cluster))
		w.End()
		w.PutUint(tlv.Context(1), uint64(version))
		w.End()
	}
}

// A controller that already has a cluster's current version gets nothing of it, and one with an
// older version gets it all, so a reconnecting controller reads only what changed.
func TestADataVersionFilterSkipsAClusterTheControllerHas(t *testing.T) {
	node, c := onOffNode()
	client, _ := pair(t, node)
	current := c.DataVersion()

	reports, err := readFiltered(t, client, filter(1, 6, current))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reports {
		if r.Path.Cluster == 6 {
			t.Fatalf("got %+v for a cluster at the version the controller has", r)
		}
	}

	reports, err = readFiltered(t, client, filter(1, 6, current-1))
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 {
		t.Fatal("nothing for a cluster at an older version than the controller's")
	}

	// A filter for another endpoint leaves this one alone.
	if reports, err := readFiltered(t, client, filter(2, 6, current)); err != nil || len(reports) == 0 {
		t.Fatalf("a filter for another endpoint: %+v, %v", reports, err)
	}
}

// A filter missing its version is malformed: the whole read is refused.
func TestADataVersionFilterWithoutAVersionIsRefused(t *testing.T) {
	node, _ := onOffNode()
	client, _ := pair(t, node)
	_, err := readFiltered(t, client, func(w *tlv.Writer) {
		w.StartStructure(tlv.Anonymous)
		w.StartList(tlv.Context(0))
		w.PutUint(tlv.Context(1), 1)
		w.PutUint(tlv.Context(2), 6)
		w.End()
		w.End()
	})
	if err == nil {
		t.Fatal("a read with a filter missing its version was answered")
	}
}

// outcomes is an Observer that records every write's and invoke's outcome.
type outcomes struct {
	mu      sync.Mutex
	wrote   []Status
	invoked []Status
}

func (o *outcomes) Wrote(cluster uint32, st Status) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cluster == 0 {
		o.wrote = append(o.wrote, st)
	}
}

func (o *outcomes) Invoked(cluster uint32, st Status) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cluster == 0 {
		o.invoked = append(o.invoked, st)
	}
}
func (*outcomes) Reported()       {}
func (*outcomes) Panicked(string) {}

// send sends a raw message on a new exchange and returns the status it's answered with.
func send(t *testing.T, c *Client, op uint8, payload []byte) Status {
	t.Helper()
	ctx := timeoutCtx(t)
	ex, err := c.Session.NewExchange()
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	if err := ex.Send(ctx, imMessage(op, payload)); err != nil {
		t.Fatal(err)
	}
	m, err := ex.Recv(ctx)
	if err != nil || m.Opcode != OpStatusResponse {
		t.Fatalf("got %+v, %v, want a status response", m, err)
	}
	st, err := decodeStatusResponse(m.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A write or invoke refused as a whole is answered with its status, and counted against no
// cluster.
func TestARefusedInteractionIsAnsweredAndCounted(t *testing.T) {
	node, _ := onOffNode()
	o := &outcomes{}
	client, _, _, _ := pairWith(t, &Server{Node: node, Observer: o, Log: logging.Test(t)}, transport.SecureSessionConfig{Auth: transport.AuthPASE})

	timedWrite := func() []byte { // says it's timed, with no Timed Request before it
		var w tlv.Writer
		w.StartStructure(tlv.Anonymous)
		w.PutBool(tlv.Context(1), true)
		w.StartArray(tlv.Context(2))
		w.End()
		w.PutUint(tagRevision, revision)
		w.End()
		return w.Bytes()
	}
	invoke := func(timed bool, commands int) []byte {
		var w tlv.Writer
		w.StartStructure(tlv.Anonymous)
		w.PutBool(tlv.Context(0), false)
		w.PutBool(tlv.Context(1), timed)
		w.StartArray(tlv.Context(2))
		for range commands {
			w.StartStructure(tlv.Anonymous)
			w.StartList(tlv.Context(0))
			w.PutUint(tlv.Context(0), 1)
			w.PutUint(tlv.Context(1), 6)
			w.PutUint(tlv.Context(2), 1)
			w.End()
			w.StartStructure(tlv.Context(1))
			w.End()
			w.End()
		}
		w.End()
		w.PutUint(tagRevision, revision)
		w.End()
		return w.Bytes()
	}

	for name, tt := range map[string]struct {
		op   uint8
		msg  []byte
		want Status
	}{
		"a write that says it's timed":   {OpWriteRequest, timedWrite(), StatusTimedRequestMismatch},
		"a write that isn't TLV":         {OpWriteRequest, []byte{0xFF}, StatusInvalidAction},
		"an invoke that says it's timed": {OpInvokeRequest, invoke(true, 1), StatusTimedRequestMismatch},
		"an invoke of two commands":      {OpInvokeRequest, invoke(false, 2), StatusInvalidAction},
	} {
		if st := send(t, client, tt.op, tt.msg); st != tt.want {
			t.Errorf("%s: status %#x, want %#x", name, st, tt.want)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.wrote) != 2 || len(o.invoked) != 2 {
		t.Errorf("observed writes %v and invokes %v refused, want two of each", o.wrote, o.invoked)
	}
}
