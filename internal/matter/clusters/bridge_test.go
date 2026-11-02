package clusters

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	mcrypto "github.com/harrymitchinson/oikade/internal/matter/crypto"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

const passcode = 20202021

var salt = []byte("SPAKE2P Key Salt")

type fixture struct {
	bridge  *Bridge
	onoff   *OnOff
	info    *BridgedInfo
	server  *im.Server
	client  *im.Client
	session *transport.Session
	changes atomic.Int32 // OnChange calls
	devTr   *transport.Transport
	cliTr   *transport.Transport
}

func serve(t *testing.T) *transport.Transport {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr := transport.New(pc, transport.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tr
}

// newFixture is a bridge with one on/off plug on endpoint 2, served over a PASE session.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{devTr: serve(t), cliTr: serve(t)}
	f.bridge = NewBridge(Info{
		VendorName: "Oikade", VendorID: 0xFFF1, ProductName: "Bridge", ProductID: 0x8000,
		NodeLabel: "Oikade", SoftwareVersion: 1, SoftwareVersionString: "0.1.0", UniqueID: "bridge-1",
	})
	f.onoff = NewOnOff()
	f.onoff.OnChange = func(bool) { f.changes.Add(1) }
	var err error
	if f.info, err = f.bridge.AddDevice(2, DeviceOnOffPlug, "Desk lamp", "lamp-1", f.onoff.Cluster()); err != nil {
		t.Fatal(err)
	}
	f.server = &im.Server{Node: f.bridge.Node, Log: testr.New(t)}
	f.server.Register(f.devTr)

	v, err := mcrypto.NewVerifier(passcode, salt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	(&securechannel.Responder{Transport: f.devTr, Verifier: v, Salt: salt, Iterations: 1000, Log: testr.New(t)}).Register()
	devAddr := f.devTr.Addr().(*net.UDPAddr).AddrPort()
	f.session, err = securechannel.EstablishPASE(timeout(t), f.cliTr, devAddr, passcode, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.client = &im.Client{Session: f.session}
	f.client.Register(f.cliTr)
	return f
}

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func value(t *testing.T, r im.AttributeReport) any {
	t.Helper()
	if r.Status != im.StatusSuccess || r.Data == nil {
		t.Fatalf("%+v: status %#x", r.Path, r.Status)
	}
	v, err := im.DecodeValue(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func find(reports []im.AttributeReport, p im.Path) *im.AttributeReport {
	for i, r := range reports {
		if r.Path.Endpoint == p.Endpoint && r.Path.Cluster == p.Cluster && r.Path.Attribute == p.Attribute {
			return &reports[i]
		}
	}
	return nil
}

func wildcard() im.AttributePath {
	return im.AttributePath{WildEndpoint: true, WildCluster: true, WildAttribute: true}
}

// TestReadEverything reads every attribute of every endpoint with one wildcard read, which
// takes several chunks.
func TestReadEverything(t *testing.T) {
	f := newFixture(t)
	reports, err := f.client.Read(timeout(t), wildcard())
	if err != nil {
		t.Fatal(err)
	}

	// Every attribute every cluster lists is there, once.
	seen := map[im.Path]bool{}
	for _, r := range reports {
		p := im.Path{Endpoint: r.Path.Endpoint, Cluster: r.Path.Cluster, Attribute: r.Path.Attribute}
		if seen[p] {
			t.Errorf("%+v reported twice", p)
		}
		seen[p] = true
		if r.Status != im.StatusSuccess {
			t.Errorf("%+v: status %#x", p, r.Status)
		}
	}
	for _, r := range reports {
		if r.Path.Attribute != im.AttrAttributeList {
			continue
		}
		for _, a := range value(t, r).([]any) {
			p := im.Path{Endpoint: r.Path.Endpoint, Cluster: r.Path.Cluster, Attribute: uint32(a.(uint64))}
			if !seen[p] {
				t.Errorf("%+v is in AttributeList but wasn't reported", p)
			}
		}
	}
	if len(reports) < 40 {
		t.Errorf("only %d reports", len(reports))
	}

	parts := func(ep uint16) []any {
		return value(t, *find(reports, im.Path{Endpoint: ep, Cluster: IDDescriptor, Attribute: 3})).([]any)
	}
	if got := parts(0); !slices.Equal(got, []any{uint64(1), uint64(2)}) {
		t.Errorf("root PartsList = %v, want [1 2]", got)
	}
	if got := parts(1); !slices.Equal(got, []any{uint64(2)}) {
		t.Errorf("aggregator PartsList = %v, want [2]", got)
	}
	types := value(t, *find(reports, im.Path{Endpoint: 2, Cluster: IDDescriptor, Attribute: 0})).([]any)
	if len(types) != 2 || types[0].(map[uint8]any)[0] != uint64(DeviceOnOffPlug.ID) || types[1].(map[uint8]any)[0] != uint64(DeviceBridgedNode.ID) {
		t.Errorf("bridged DeviceTypeList = %v", types)
	}
	servers := value(t, *find(reports, im.Path{Endpoint: 2, Cluster: IDDescriptor, Attribute: 1})).([]any)
	if !slices.Equal(servers, []any{uint64(IDOnOff), uint64(IDDescriptor), uint64(IDBridgedDeviceBasicInformation)}) {
		t.Errorf("bridged ServerList = %v", servers)
	}
	if got := value(t, *find(reports, im.Path{Endpoint: 0, Cluster: IDBasicInformation, Attribute: 1})); got != "Oikade" {
		t.Errorf("VendorName = %v", got)
	}
	if got := value(t, *find(reports, im.Path{Endpoint: 2, Cluster: IDBridgedDeviceBasicInformation, Attribute: 0x11})); got != true {
		t.Errorf("Reachable = %v", got)
	}
	accepted := value(t, *find(reports, im.Path{Endpoint: 2, Cluster: IDOnOff, Attribute: im.AttrAcceptedCommandList})).([]any)
	if !slices.Equal(accepted, []any{uint64(0), uint64(1), uint64(2)}) {
		t.Errorf("AcceptedCommandList = %v", accepted)
	}
}

func TestToggle(t *testing.T) {
	f := newFixture(t)
	path := im.Path{Endpoint: 2, Cluster: IDOnOff, Attribute: 0}
	read := func() any {
		reports, err := f.client.Read(timeout(t), im.Concrete(path))
		if err != nil || len(reports) != 1 {
			t.Fatalf("Read: %v, %v", reports, err)
		}
		return value(t, reports[0])
	}
	if read() != false {
		t.Fatal("starts on")
	}
	res, err := f.client.Invoke(timeout(t), im.CommandPath{Endpoint: 2, Cluster: IDOnOff, Command: 2}, nil, false, 0)
	if err != nil || res.Status != im.StatusSuccess {
		t.Fatalf("Toggle: %+v, %v", res, err)
	}
	if read() != true || f.changes.Load() != 1 {
		t.Errorf("after Toggle: %v, %d changes", read(), f.changes.Load())
	}
}

func TestSubscription(t *testing.T) {
	f := newFixture(t)
	path := im.Path{Endpoint: 2, Cluster: IDOnOff, Attribute: 0}
	priming, id, maxInterval, reports, err := f.client.Subscribe(timeout(t), 0, 1,
		im.AttributePath{Endpoint: 2, WildCluster: true, WildAttribute: true})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 || maxInterval != 1 || find(priming, path) == nil || f.server.Subscriptions() != 1 {
		t.Fatalf("subscription %d, max %d s, %d priming reports, %d subscriptions", id, maxInterval, len(priming), f.server.Subscriptions())
	}

	// A change from the device side is reported.
	f.bridge.Node.Update(func() { f.onoff.Set(true) })
	select {
	case got := <-reports:
		r := find(got, path)
		if r == nil || value(t, *r) != true {
			t.Errorf("report %+v, want OnOff true", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no report of the change")
	}

	// Idle, it sends empty keep-alive reports every max interval.
	start := time.Now()
	select {
	case got := <-reports:
		if len(got) != 0 {
			t.Errorf("keep-alive with %d reports", len(got))
		}
		if d := time.Since(start); d > 1500*time.Millisecond {
			t.Errorf("keep-alive after %v, max interval 1 s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no keep-alive")
	}

	// A change to the bridged info is reported too.
	f.bridge.Node.Update(func() { f.info.SetReachable(false) })
	select {
	case got := <-reports:
		r := find(got, im.Path{Endpoint: 2, Cluster: IDBridgedDeviceBasicInformation, Attribute: 0x11})
		if r == nil || value(t, *r) != false {
			t.Errorf("report %+v, want Reachable false", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no report of Reachable")
	}
}

func TestSubscriptionEndsWithSession(t *testing.T) {
	f := newFixture(t)
	if _, _, _, _, err := f.client.Subscribe(timeout(t), 0, 1, im.Concrete(im.Path{Endpoint: 2, Cluster: IDOnOff})); err != nil {
		t.Fatal(err)
	}
	f.cliTr.RemoveSession(f.session) // the client goes away and stops answering
	deadline := time.Now().Add(15 * time.Second)
	for f.server.Subscriptions() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if f.server.Subscriptions() != 0 {
		t.Error("the subscription outlived its client")
	}
}

func TestUnsupportedPaths(t *testing.T) {
	f := newFixture(t)
	for want, p := range map[im.Status]im.AttributePath{
		im.StatusUnsupportedEndpoint:  {Endpoint: 9, Cluster: IDOnOff},
		im.StatusUnsupportedCluster:   {Endpoint: 2, Cluster: 0x0300},
		im.StatusUnsupportedAttribute: {Endpoint: 2, Cluster: IDOnOff, Attribute: 0x4000},
	} {
		reports, err := f.client.Read(timeout(t), p)
		if err != nil || len(reports) != 1 || reports[0].Status != want {
			t.Errorf("read %+v: %+v, %v; want status %#x", p, reports, err, want)
		}
	}
	for want, p := range map[im.Status]im.CommandPath{
		im.StatusUnsupportedEndpoint: {Endpoint: 9, Cluster: IDOnOff},
		im.StatusUnsupportedCluster:  {Endpoint: 2, Cluster: 0x0300},
		im.StatusUnsupportedCommand:  {Endpoint: 2, Cluster: IDOnOff, Command: 0x40},
	} {
		res, err := f.client.Invoke(timeout(t), p, nil, false, 0)
		if err != nil || res.Status != want {
			t.Errorf("invoke %+v: %+v, %v; want status %#x", p, res, err, want)
		}
	}
	// A wildcard skips what's missing instead.
	reports, err := f.client.Read(timeout(t), im.AttributePath{WildEndpoint: true, Cluster: IDOnOff, Attribute: 0})
	if err != nil || len(reports) != 1 || reports[0].Path.Endpoint != 2 {
		t.Errorf("wildcard read of OnOff: %+v, %v", reports, err)
	}
}

func TestWrite(t *testing.T) {
	f := newFixture(t)
	label := im.AttributePath{Endpoint: 0, Cluster: IDBasicInformation, Attribute: 5}
	putString := func(s string) func(w *tlv.Writer, tag tlv.Tag) {
		return func(w *tlv.Writer, tag tlv.Tag) { w.PutUTF8(tag, s) }
	}
	if st, err := f.client.Write(timeout(t), label, putString("Living room bridge")); err != nil || st != im.StatusSuccess {
		t.Fatalf("Write NodeLabel: %#x, %v", st, err)
	}
	reports, _ := f.client.Read(timeout(t), label)
	if v := value(t, reports[0]); v != "Living room bridge" {
		t.Errorf("NodeLabel = %v", v)
	}
	for want, c := range map[im.Status]struct {
		path  im.AttributePath
		value func(w *tlv.Writer, tag tlv.Tag)
	}{
		im.StatusConstraintError:  {label, putString("a label that is longer than thirty-two bytes")},
		im.StatusInvalidDataType:  {label, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, 1) }},
		im.StatusUnsupportedWrite: {im.AttributePath{Endpoint: 2, Cluster: IDOnOff, Attribute: 0}, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) }},
		im.StatusInvalidAction:    {im.AttributePath{WildEndpoint: true, Cluster: IDBasicInformation, Attribute: 5}, putString("x")},
	} {
		if st, err := f.client.Write(timeout(t), c.path, c.value); err != nil || st != want {
			t.Errorf("write %+v: %#x, %v; want %#x", c.path, st, err, want)
		}
	}
}

func TestTimedInvoke(t *testing.T) {
	f := newFixture(t)
	res, err := f.client.Invoke(timeout(t), im.CommandPath{Endpoint: 2, Cluster: IDOnOff, Command: 1}, nil, true, 1000)
	if err != nil || res.Status != im.StatusSuccess || f.changes.Load() != 1 {
		t.Errorf("timed On: %+v, %v, %d changes", res, err, f.changes.Load())
	}
}

func TestDataVersionChanges(t *testing.T) {
	f := newFixture(t)
	path := im.Concrete(im.Path{Endpoint: 2, Cluster: IDOnOff, Attribute: 0})
	before, _ := f.client.Read(timeout(t), path)
	f.bridge.Node.Update(func() { f.onoff.Set(true) })
	after, _ := f.client.Read(timeout(t), path)
	if before[0].Version == after[0].Version {
		t.Error("the data version didn't change with the value")
	}
}

func TestRefusesUnsecuredSession(t *testing.T) {
	f := newFixture(t)
	unsecured := f.cliTr.NewUnsecuredSession(f.devTr.Addr().(*net.UDPAddr).AddrPort())
	c := &im.Client{Session: unsecured}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := c.Read(ctx, wildcard()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("read over an unsecured session: %v, want no answer", err)
	}
}

func TestLocationMustBeTwoLetters(t *testing.T) {
	f := newFixture(t)
	loc := im.AttributePath{Endpoint: 0, Cluster: IDBasicInformation, Attribute: 6}
	for v, want := range map[string]im.Status{"G": im.StatusConstraintError, "": im.StatusConstraintError, "GBR": im.StatusConstraintError, "GB": im.StatusSuccess} {
		st, err := f.client.Write(timeout(t), loc, func(w *tlv.Writer, tag tlv.Tag) { w.PutUTF8(tag, v) })
		if err != nil || st != want {
			t.Errorf("Location %q: %#x, %v; want %#x", v, st, err, want)
		}
	}
}

func TestDuplicateEndpoint(t *testing.T) {
	b := NewBridge(Info{})
	if _, err := b.AddDevice(2, DeviceOnOffPlug, "a", "a", NewOnOff().Cluster()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint16{0, 1, 2} {
		if _, err := b.AddDevice(id, DeviceOnOffPlug, "b", "b", NewOnOff().Cluster()); err == nil {
			t.Errorf("endpoint %d added twice", id)
		}
	}
}

// TestSubscriberSeesNewDevice adds a device at runtime: a wildcard subscriber hears of its
// attributes and the PartsLists that now include it.
func TestSubscriberSeesNewDevice(t *testing.T) {
	f := newFixture(t)
	_, _, _, reports, err := f.client.Subscribe(timeout(t), 0, 60, wildcard())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.bridge.AddDevice(3, DeviceOnOffLight, "Hall light", "hall-1", NewOnOff().Cluster()); err != nil {
		t.Fatal(err)
	}
	var got []im.AttributeReport
	deadline := time.After(3 * time.Second)
	for find(got, im.Path{Endpoint: 3, Cluster: IDOnOff, Attribute: 0}) == nil || find(got, im.Path{Endpoint: 1, Cluster: IDDescriptor, Attribute: 3}) == nil {
		select {
		case r := <-reports:
			got = append(got, r...)
		case <-deadline:
			t.Fatalf("reports after adding a device: %+v", got)
		}
	}
	if parts := value(t, *find(got, im.Path{Endpoint: 1, Cluster: IDDescriptor, Attribute: 3})).([]any); !slices.Equal(parts, []any{uint64(2), uint64(3)}) {
		t.Errorf("aggregator PartsList = %v, want [2 3]", parts)
	}
	if find(got, im.Path{Endpoint: 0, Cluster: IDDescriptor, Attribute: 3}) == nil {
		t.Error("the root's PartsList wasn't reported")
	}
}
