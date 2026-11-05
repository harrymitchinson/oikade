package status

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"

	"github.com/go-logr/logr"
)

func TestDiscoveredPatchesOnlyChanges(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pluginsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	p := &pluginsv1alpha1.Plugin{ObjectMeta: metav1.ObjectMeta{Name: "hue", Namespace: "home"}}
	patches := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p).WithStatusSubresource(p).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				patches++
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	store := hub.NewStore(logr.Discard())
	w := &Discovered{Client: c, Store: store}
	nn := types.NamespacedName{Namespace: "home", Name: "hue"}
	key := driver.InstanceKey{Driver: driver.Plugin, NamespacedName: nn}
	sync := func() {
		t.Helper()
		if _, err := w.reconcilePlugin(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
			t.Fatal(err)
		}
	}

	// Nothing to write before the plugin lists devices.
	sync()
	store.Inventory(key, []driver.AppDevice{{ID: "a", Name: "Desk lamp", Capabilities: []string{"on-off"}}})
	sync()
	sync()
	if patches != 1 {
		t.Errorf("patched %d times, want 1", patches)
	}
	var got pluginsv1alpha1.Plugin
	if err := c.Get(context.Background(), nn, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Discovered) != 1 || got.Status.Discovered[0].Name != "Desk lamp" {
		t.Errorf("status.discovered = %+v", got.Status.Discovered)
	}

	// An app with no devices is an empty list, kept apart from none.
	store.Inventory(key, []driver.AppDevice{})
	sync()
	if err := c.Get(context.Background(), nn, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Discovered == nil || len(got.Status.Discovered) != 0 || patches != 2 {
		t.Errorf("status.discovered = %#v after %d patches, want empty after 2", got.Status.Discovered, patches)
	}

	// No list clears the field.
	store.Inventory(key, nil)
	sync()
	if err := c.Get(context.Background(), nn, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Discovered != nil || patches != 3 {
		t.Errorf("status.discovered = %+v after %d patches, want none after 3", got.Status.Discovered, patches)
	}

	// A restarted runtime whose driver hasn't reported yet keeps the list.
	store.Inventory(key, []driver.AppDevice{{ID: "a"}})
	sync()
	w.Store = hub.NewStore(logr.Discard())
	sync()
	if err := c.Get(context.Background(), nn, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Discovered) != 1 || patches != 4 {
		t.Errorf("status.discovered = %+v after %d patches, want kept after 4", got.Status.Discovered, patches)
	}
}
