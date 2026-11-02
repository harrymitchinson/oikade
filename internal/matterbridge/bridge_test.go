package matterbridge

import (
	"slices"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/matter/clusters"
	"github.com/harrymitchinson/oikade/internal/matter/im"
)

func device(name string, caps ...string) *homev1alpha1.Device {
	d := &homev1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "home"}}
	for _, c := range caps {
		d.Status.Capabilities = append(d.Status.Capabilities, homev1alpha1.DeviceCapability{Name: c})
	}
	return d
}

func newBridgeRig(t *testing.T, objs ...client.Object) (*Bridge, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, homev1alpha1.AddToScheme, matterv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	br := &matterv1alpha1.Bridge{ObjectMeta: metav1.ObjectMeta{Name: "upstairs", Namespace: "home"}}
	br.Status.Endpoints = []matterv1alpha1.Endpoint{{Device: "desk-lamp", Endpoint: 2}, {Device: "hall-sensor", Endpoint: 3}, {Device: "router", Endpoint: 4}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, br)...).WithStatusSubresource(br, &homev1alpha1.Device{}).Build()
	b := &Bridge{Client: c, Key: upstairs, Matter: clusters.NewBridge(clusters.Info{})}
	b.Store = hub.NewStore(logr.Discard(), b)
	b.Writes = &Coalescer{Setter: &recorder{}, Current: func(types.NamespacedName, string) (any, bool) { return nil, false }}
	return b, c
}

func endpointIDs(b *Bridge) []uint16 {
	var ids []uint16
	b.Matter.Node.View(func() {
		for _, e := range b.Matter.Node.Endpoints() {
			ids = append(ids, e.ID)
		}
	})
	return ids
}

func TestBridgeFollowsDevices(t *testing.T) {
	lamp := device("desk-lamp", capability.LightOn, capability.LightLevel)
	b, c := newBridgeRig(t, lamp, device("hall-sensor", capability.Occupancy), device("router", capability.LinkQuality),
		device("unallocated", capability.SwitchOn))
	if _, err := b.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	// The router has nothing Matter shows, and the unallocated Device has no endpoint yet.
	if got := endpointIDs(b); len(got) != 4 || got[2] != 2 || got[3] != 3 {
		t.Fatalf("endpoints %v, want 0 1 2 3", got)
	}

	// The bus reaches the clusters, and availability is Reachable.
	b.Store.SetDevices(driver.InstanceKey{}, map[types.NamespacedName]hub.Meta{
		{Namespace: "home", Name: "desk-lamp"}: {},
	})
	b.Changed(driver.Update{Device: types.NamespacedName{Namespace: "home", Name: "desk-lamp"}, Capability: capability.LightOn, Value: true})
	b.Availability(types.NamespacedName{Namespace: "home", Name: "desk-lamp"}, false)
	b.Matter.Node.View(func() {
		e := b.Matter.Node.Endpoint(2)
		var on, reachable bool
		on = readAttr(t, e, clusters.IDOnOff, 0).(bool)
		reachable = readAttr(t, e, clusters.IDBridgedDeviceBasicInformation, 0x11).(bool)
		if !on || reachable {
			t.Errorf("OnOff %v, Reachable %v", on, reachable)
		}
	})

	// A Device that changes is rebuilt on its endpoint; one that goes away is removed.
	lamp.Status.Capabilities = lamp.Status.Capabilities[:1]
	if err := c.Status().Update(t.Context(), lamp); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), device("hall-sensor")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := endpointIDs(b); len(got) != 3 || got[2] != 2 {
		t.Fatalf("endpoints %v, want 0 1 2", got)
	}
	b.Matter.Node.View(func() {
		if b.Matter.Node.Endpoint(2).Cluster(clusters.IDLevelControl) != nil {
			t.Error("the lamp kept Level Control after it lost light.level")
		}
	})
}

func readAttr(t *testing.T, e *im.Endpoint, cluster, attr uint32) any {
	t.Helper()
	return read(t, e.Cluster(cluster), attr)
}

// Only changes that can move endpoints wake the bridge, not each reading's status update.
func TestDeviceShapeChanged(t *testing.T) {
	old := device("desk-lamp", capability.LightOn, capability.LightLevel)
	for name, change := range map[string]struct {
		edit func(d *homev1alpha1.Device)
		want bool
	}{
		"reading":    {func(d *homev1alpha1.Device) { d.Status.Capabilities[0].Kind = "number" }, false},
		"same names": {func(d *homev1alpha1.Device) { slices.Reverse(d.Status.Capabilities) }, false},
		"capability added": {func(d *homev1alpha1.Device) {
			d.Status.Capabilities = append(d.Status.Capabilities, homev1alpha1.DeviceCapability{Name: capability.Battery})
		}, true},
		"label":      {func(d *homev1alpha1.Device) { d.Labels = map[string]string{"room": "study"} }, true},
		"generation": {func(d *homev1alpha1.Device) { d.Generation++ }, true},
	} {
		n := old.DeepCopy()
		change.edit(n)
		if got := deviceShapeChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: n}); got != change.want {
			t.Errorf("%s: %v, want %v", name, got, change.want)
		}
	}
}

// Changed never blocks, however often the node calls it before Run starts.
func TestFabricsChangedDoesNotBlock(t *testing.T) {
	p := &FabricsPublisher{}
	for range 3 {
		p.Changed()
	}
}
