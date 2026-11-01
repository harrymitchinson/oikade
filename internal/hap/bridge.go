package hap

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	homekitv1alpha1 "github.com/harrymitchinson/oikade/api/homekit/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/identity"
)

// setTimeout bounds how long a Home app write waits for the bus.
const setTimeout = 5 * time.Second

// Bridge runs the HAP server for one Bridge object. It is a driver.Sink that pushes capability
// changes to HomeKit, and a controller that rebuilds the server when what HomeKit sees changes.
type Bridge struct {
	hub.NopSink
	client.Client
	// APIReader reads Secrets directly; the pod's Role can't list or watch them.
	APIReader client.Reader
	Key       types.NamespacedName
	Store     *hub.Store
	Setter    driver.Setter
	DataDir   string

	ctx         context.Context
	mu          sync.Mutex
	signature   string
	accessories map[types.NamespacedName]*Accessory
	stop        context.CancelFunc
	done        chan struct{}
}

// Changed implements driver.Sink.
func (b *Bridge) Changed(u driver.Update) {
	b.mu.Lock()
	a := b.accessories[u.Device]
	b.mu.Unlock()
	if a != nil {
		a.Update(u.Capability, u.Value)
	}
}

// Reconcile rebuilds the HAP server if the bridge's identity, setup code or accessories changed.
func (b *Bridge) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var br homekitv1alpha1.Bridge
	if err := b.Get(ctx, b.Key, &br); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		b.shutdown()
		return ctrl.Result{}, nil
	}
	if br.Status.Port == 0 || !identity.ValidUsername(br.Status.Username) {
		log.Info("Waiting for the operator to assign a port and identity", "bridge", b.Key)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	pin, err := b.readPin(ctx, &br)
	if err != nil {
		return ctrl.Result{}, err
	}

	devices, err := b.devices(ctx, &br)
	if err != nil {
		return ctrl.Result{}, err
	}
	sig := []string{fmt.Sprint(br.Status.Port), br.Status.Username, pin, br.Title()}
	for _, d := range devices {
		sig = append(sig, d.Signature())
	}
	signature := strings.Join(sig, "\n")

	b.mu.Lock()
	unchanged := signature == b.signature
	b.mu.Unlock()
	if unchanged {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, b.restart(&br, pin, devices, signature)
}

// devices lists the selected Devices that have an AID and something HomeKit can show.
func (b *Bridge) devices(ctx context.Context, br *homekitv1alpha1.Bridge) ([]Device, error) {
	selector, err := metav1.LabelSelectorAsSelector(&br.Spec.DeviceSelector)
	if err != nil {
		return nil, err
	}
	var list homev1alpha1.DeviceList
	if err := b.List(ctx, &list, client.InNamespace(br.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	var out []Device
	for _, d := range list.Items {
		aid := br.Status.AID(d.Name)
		if aid == 0 {
			continue // the operator hasn't assigned one yet
		}
		dev := Device{
			Key: client.ObjectKeyFromObject(&d), AID: aid, Name: d.Title(), Serial: d.Namespace + "/" + d.Name,
			Manufacturer: d.Status.Manufacturer, Model: d.Status.Model,
		}
		// Capabilities come from status, not the live store, so the accessory list is the same
		// from the moment the pod starts and HomeKit doesn't see devices vanish and reappear.
		for _, c := range d.Status.Capabilities {
			dev.Capabilities = append(dev.Capabilities, c.Name)
		}
		if d.Spec.HomeKit != nil {
			dev.Service = d.Spec.HomeKit.Service
		}
		out = append(out, dev)
	}
	slices.SortFunc(out, func(x, y Device) int { return int(x.AID) - int(y.AID) })
	return out, nil
}

func (b *Bridge) readPin(ctx context.Context, br *homekitv1alpha1.Bridge) (string, error) {
	var s corev1.Secret
	ref := br.Spec.PinSecretRef
	if err := b.APIReader.Get(ctx, types.NamespacedName{Namespace: br.Namespace, Name: ref.Name}, &s); err != nil {
		return "", fmt.Errorf("reading setup code: %w", err)
	}
	pin := string(s.Data[ref.Key])
	if err := identity.ValidatePin(pin); err != nil {
		return "", err
	}
	return pin, nil
}

// restart stops the running server, if any, and starts one for the new accessory set. hap bumps
// the configuration number itself when the accessories change, so iPhones refresh.
func (b *Bridge) restart(br *homekitv1alpha1.Bridge, pin string, devices []Device, signature string) error {
	log := logf.FromContext(b.ctx)
	b.shutdown()

	accessories := map[types.NamespacedName]*Accessory{}
	var as []*accessory.A
	for _, d := range devices {
		if snap, ok := b.Store.Get(d.Key); ok {
			d.Values = snap.Values
		}
		a := Build(d, b.setter(d.Key))
		if a == nil {
			continue
		}
		accessories[d.Key] = a
		as = append(as, a.A)
	}

	bridge := accessory.NewBridge(accessory.Info{Name: br.Title(), Manufacturer: "Oikade", Model: "Bridge", SerialNumber: br.Name})
	bridge.Id = 1
	store := hap.NewFsStore(b.DataDir)
	// hap generates its own identity on first start; pin it to the one the operator keeps.
	if err := store.Set("uuid", []byte(br.Status.Username)); err != nil {
		return fmt.Errorf("writing bridge identity: %w", err)
	}
	server, err := hap.NewServer(store, bridge.A, as...)
	if err != nil {
		return fmt.Errorf("creating HAP server: %w", err)
	}
	if err := fixListPairings(server, store); err != nil {
		return fmt.Errorf("wrapping HAP pairings: %w", err)
	}
	server.Pin = Pin(pin)
	server.Addr = fmt.Sprintf(":%d", br.Status.Port)

	ctx, cancel := context.WithCancel(b.ctx)
	done := make(chan struct{})
	b.mu.Lock()
	b.signature, b.accessories, b.stop, b.done = signature, accessories, cancel, done
	b.mu.Unlock()

	go func() {
		defer close(done)
		log.Info("Starting HAP server", "bridge", b.Key, "port", br.Status.Port, "accessories", len(as))
		if err := server.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "HAP server stopped", "bridge", b.Key)
		}
	}()
	return nil
}

// shutdown stops the running server and waits for its port to be released.
func (b *Bridge) shutdown() {
	b.mu.Lock()
	stop, done := b.stop, b.done
	b.signature, b.accessories, b.stop, b.done = "", nil, nil, nil
	b.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

func (b *Bridge) setter(dev types.NamespacedName) SetFunc {
	return func(name string, value any) error {
		ctx, cancel := context.WithTimeout(b.ctx, setTimeout)
		defer cancel()
		err := b.Setter.Set(ctx, dev, name, value)
		if err != nil {
			logf.FromContext(b.ctx).Error(err, "Failed to set capability from HomeKit", "device", dev, "capability", name)
		}
		return err
	}
}

// SetupWithManager registers the bridge controller. Every request maps to the one Bridge.
func (b *Bridge) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	b.ctx = ctx
	toBridge := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: b.Key}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("hap-bridge").
		Watches(&homekitv1alpha1.Bridge{}, toBridge).
		Watches(&homev1alpha1.Device{}, toBridge).
		Complete(b)
}
