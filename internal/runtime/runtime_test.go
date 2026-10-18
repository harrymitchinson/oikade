/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runtime_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	homev1alpha1 "github.com/harry/oikade/api/home/v1alpha1"
	zigbeev1alpha1 "github.com/harry/oikade/api/zigbee/v1alpha1"
	"github.com/harry/oikade/internal/capability"
	"github.com/harry/oikade/internal/mqtt"
	"github.com/harry/oikade/internal/mqtt/mqtttest"
	"github.com/harry/oikade/internal/runtime"
)

const ns = "home"

func TestRuntimeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinaries(t),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := kruntime.NewScheme()
	for _, add := range []func(*kruntime.Scheme) error{clientgoscheme.AddToScheme, homev1alpha1.AddToScheme, zigbeev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	url := mqtttest.StartBroker(t)
	bus, err := mqtt.Connect(ctx, url, "runtime")
	if err != nil {
		t.Fatal(err)
	}
	fakeZ2M, err := mqtt.Connect(ctx, url, "fake-z2m")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	if _, err := fakeZ2M.Subscribe(ctx, "z2m/+/set", func(m mqtt.Message) {
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, m.Topic+" "+string(m.Payload))
	}); err != nil {
		t.Fatal(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	if _, err := runtime.Setup(ctx, mgr, bus, runtime.Options{
		Location: time.UTC, SeenInterval: time.Minute, TriggeredInterval: time.Second, Registry: reg,
	}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	// The declared home.
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	must(t, c.Create(ctx, &zigbeev1alpha1.Zigbee2MQTT{
		ObjectMeta: metav1.ObjectMeta{Name: "z2m", Namespace: ns},
		Spec:       zigbeev1alpha1.Zigbee2MQTTSpec{Serial: zigbeev1alpha1.SerialSpec{Port: "tcp://slzb:6638", Adapter: "zstack"}},
	}))
	device := func(name, ieee string) *homev1alpha1.Device {
		return &homev1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: homev1alpha1.DeviceSpec{
				RoomRef: &corev1.LocalObjectReference{Name: "hallway"},
				Zigbee2MQTT: &homev1alpha1.ZigbeeDeviceSource{
					InstanceRef: corev1.LocalObjectReference{Name: "z2m"}, IEEEAddress: ieee,
				},
			},
		}
	}
	must(t, c.Create(ctx, device("hallway-light", "0x000b57fffe000001")))
	must(t, c.Create(ctx, device("hallway-motion", "0x00158d0000000002")))
	must(t, c.Create(ctx, device("not-paired", "0x00158d00000000ff")))
	must(t, c.Create(ctx, &homev1alpha1.Automation{
		ObjectMeta: metav1.ObjectMeta{Name: "motion-light", Namespace: ns},
		Spec: homev1alpha1.AutomationSpec{
			Triggers:  []homev1alpha1.Trigger{{Device: "hallway-motion", Capability: capability.Occupancy}},
			Condition: "trigger.value",
			Actions: []homev1alpha1.Action{{Set: &homev1alpha1.SetAction{
				Device: "hallway-light", Capability: capability.LightOn, Value: apiextensionsv1.JSON{Raw: []byte("true")},
			}}},
		},
	}))

	// Zigbee2MQTT announces its paired devices.
	devices, err := os.ReadFile(filepath.Join("..", "driver", "zigbee2mqtt", "testdata", "bridge-devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, fakeZ2M.Publish(ctx, "z2m/bridge/devices", devices, true))
	must(t, fakeZ2M.Publish(ctx, "z2m/hallway-motion/availability", []byte(`{"state":"online"}`), true))

	eventually(t, "light status", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-light"}, &d) != nil {
			return false
		}
		var caps []string
		for _, c := range d.Status.Capabilities {
			caps = append(caps, c.Name)
		}
		return meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionResolved) &&
			d.Status.Manufacturer == "IKEA" &&
			slices.Equal(caps, []string{capability.LightOn, capability.LightLevel, capability.LinkQuality})
	})
	eventually(t, "unpaired device reported", func() bool {
		var d homev1alpha1.Device
		if c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "not-paired"}, &d) != nil {
			return false
		}
		cond := meta.FindStatusCondition(d.Status.Conditions, homev1alpha1.ConditionResolved)
		return cond != nil && cond.Reason == "NotPaired"
	})
	eventually(t, "motion sensor online", func() bool {
		var d homev1alpha1.Device
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "hallway-motion"}, &d) == nil &&
			meta.IsStatusConditionTrue(d.Status.Conditions, homev1alpha1.ConditionAvailable)
	})

	// Motion turns the light on through the automation.
	must(t, fakeZ2M.Publish(ctx, "z2m/hallway-motion", []byte(`{"occupancy":true,"temperature":19.5}`), false))
	eventually(t, "light command", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(commands, `z2m/0x000b57fffe000001/set {"state":"ON"}`)
	})

	labels := prometheus.Labels{"namespace": ns, "device": "hallway-motion", "room": "hallway"}
	eventually(t, "metrics", func() bool {
		n, err := testutil.GatherAndCount(reg, "oikade_capability_value", "oikade_device_available", "oikade_undeclared_devices")
		return err == nil && n >= 4
	})
	expectMetric(t, reg, "oikade_capability_value", labels, capability.Occupancy, 1)
	expectMetric(t, reg, "oikade_capability_value", labels, capability.Temperature, 19.5)
	expectMetric(t, reg, "oikade_capability_changes_total", labels, capability.Occupancy, 1)

	eventually(t, "lastTriggered", func() bool {
		var a homev1alpha1.Automation
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "motion-light"}, &a) == nil && !a.Status.LastTriggered.IsZero()
	})
}

func expectMetric(t *testing.T, reg *prometheus.Registry, name string, labels prometheus.Labels, capName string, want float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["capability"] != capName || got["device"] != labels["device"] ||
				got["room"] != labels["room"] || got["namespace"] != labels["namespace"] {
				continue
			}
			v := m.GetGauge().GetValue() + m.GetCounter().GetValue()
			if v != want {
				t.Fatalf("%s{capability=%s} = %v, want %v", name, capName, v, want)
			}
			return
		}
	}
	t.Fatalf("%s{capability=%s} not found", name, capName)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// envtestBinaries finds binaries installed by `make setup-envtest`, as the controller suites do.
func envtestBinaries(t *testing.T) string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Skip("envtest binaries not installed; run make setup-envtest")
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	t.Skip("envtest binaries not installed; run make setup-envtest")
	return ""
}
