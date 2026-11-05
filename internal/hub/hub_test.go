package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1 "k8s.io/api/core/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// gatedBus is a Memory bus whose Subscribe and unsubscribe block on topics under gated, standing
// in for a broker that doesn't answer.
type gatedBus struct {
	*mqtt.Memory
	gated   string
	entered chan string   // receives the filter, or "unsubscribe", as a call starts waiting
	release chan struct{} // a send lets one waiting call through
}

func (b *gatedBus) Subscribe(ctx context.Context, filter string, h mqtt.Handler) (func(), error) {
	if !strings.HasPrefix(filter, b.gated) {
		return b.Memory.Subscribe(ctx, filter, h)
	}
	b.entered <- filter
	<-b.release
	unsubscribe, err := b.Memory.Subscribe(ctx, filter, h)
	return func() {
		b.entered <- "unsubscribe"
		<-b.release
		unsubscribe()
	}, err
}

// TestBrokerWaitsDontBlockSet checks that a Plugin whose subscription or unsubscription hangs
// doesn't hold up commands to other devices or the plugin stats.
func TestBrokerWaitsDontBlockSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	scheme := kruntime.NewScheme()
	for _, add := range []func(*kruntime.Scheme) error{homev1alpha1.AddToScheme, pluginsv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	plugin := func(name string) *pluginsv1alpha1.Plugin {
		return &pluginsv1alpha1.Plugin{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: name}}
	}
	lamp := &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "lamp"},
		Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
			PluginRef: corev1.LocalObjectReference{Name: "fast"}, ID: "lamp",
		}},
	}
	pump := &homev1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: "pump"},
		Spec: homev1alpha1.DeviceSpec{Plugin: &homev1alpha1.PluginDeviceSource{
			PluginRef: corev1.LocalObjectReference{Name: "slow"}, ID: "pump",
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(plugin("fast"), plugin("slow"), lamp, pump).Build()

	mem := mqtt.NewMemory()
	fast, slow := plugin("fast").Topic(), plugin("slow").Topic()
	for topic, payload := range map[string]string{
		fast + "/status":  `{"state":"online","contract":1}`,
		fast + "/devices": `{"devices":[{"id":"lamp","found":true,"capabilities":["on-off"]}]}`,
	} {
		if err := mem.Publish(ctx, topic, []byte(payload), true); err != nil {
			t.Fatal(err)
		}
	}
	bus := &gatedBus{Memory: mem, gated: slow, entered: make(chan string), release: make(chan struct{})}
	h := &Hub{
		Client: c, Bus: bus, Store: NewStore(logr.Discard()), Now: time.Now,
		ctx: ctx, instances: map[driver.InstanceKey]instance{},
	}
	reconcile := func(name string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := h.reconcilePlugin(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "home", Name: name}})
			done <- err
		}()
		return done
	}
	if err := <-reconcile("fast"); err != nil {
		t.Fatal(err)
	}

	// Set and PluginStats must return while a reconcile of slow waits on the broker.
	unblocked := func(what string, wantPlugins int) {
		t.Helper()
		done := make(chan struct{})
		var setErr error
		var plugins int
		go func() {
			defer close(done)
			setErr = h.Set(ctx, client.ObjectKeyFromObject(lamp), capability.OnOff, true)
			plugins = len(h.PluginStats())
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Set or PluginStats blocked while %s waited on the broker", what)
		}
		if setErr != nil {
			t.Errorf("Set while %s waited: %v", what, setErr)
		}
		if plugins != wantPlugins {
			t.Errorf("PluginStats while %s waited reported %d plugins, want %d", what, plugins, wantPlugins)
		}
	}

	created := reconcile("slow")
	<-bus.entered
	unblocked("subscribe", 2)
	// slow's devices are declared before Set can reach it; slow just hasn't announced them yet.
	if err := h.Set(ctx, client.ObjectKeyFromObject(pump), capability.OnOff, true); err == nil ||
		!strings.Contains(err.Error(), "not found by plugin") {
		t.Errorf("Set on a subscribing plugin's device = %v, want not found by plugin", err)
	}
	bus.release <- struct{}{}
	if err := <-created; err != nil {
		t.Fatal(err)
	}

	if err := c.Delete(ctx, plugin("slow")); err != nil {
		t.Fatal(err)
	}
	deleted := reconcile("slow")
	<-bus.entered
	// The deleted Plugin has already left the stats.
	unblocked("unsubscribe", 1)
	bus.release <- struct{}{}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
}
