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

package render

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

func plugin() *pluginsv1alpha1.Plugin {
	p := &pluginsv1alpha1.Plugin{}
	p.Name, p.Namespace = "sonoff", "home"
	p.Spec.Image = "img"
	return p
}

func TestPluginConfig(t *testing.T) {
	out, err := PluginConfig([]contractv1.DeclaredDevice{{
		ID: "1000aabbcc", Name: "porch-light", DisplayName: "Porch light", Room: "porch",
		Parameters: map[string]string{"capability": "light.on"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"contract":1,"devices":[{"id":"1000aabbcc","name":"porch-light","displayName":"Porch light",` +
		`"room":"porch","parameters":{"capability":"light.on"}}]}`
	if string(out) != want {
		t.Errorf("devices.json = %s, want %s", out, want)
	}
	empty, err := PluginConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Devices []any }
	if err := json.Unmarshal(empty, &cfg); err != nil || cfg.Devices == nil {
		t.Errorf("no devices should render an empty list, got %s", empty)
	}
}

func TestPluginDeployment(t *testing.T) {
	d := PluginDeployment(PluginInput{Plugin: plugin(), MQTTURL: "mqtt://broker:1883", ConfigHash: "h"})
	if *d.Name != "sonoff-plugin" {
		t.Errorf("deployment name = %q, want sonoff-plugin", *d.Name)
	}
	if *d.Spec.Replicas != 1 || *d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("want one replica with Recreate, got %v %v", *d.Spec.Replicas, *d.Spec.Strategy.Type)
	}
	tmpl := d.Spec.Template
	if tmpl.Annotations[AnnotationConfigHash] != "h" {
		t.Errorf("config hash annotation = %q", tmpl.Annotations[AnnotationConfigHash])
	}
	if _, ok := tmpl.Annotations[AnnotationSecretVersion]; ok {
		t.Error("no secretRef should mean no secret version annotation")
	}
	pod := tmpl.Spec
	if pod.HostNetwork != nil || pod.DNSPolicy != nil {
		t.Error("hostNetwork should be off by default")
	}
	if *pod.AutomountServiceAccountToken || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("plugin pod should be non-root without a service account token")
	}
	c := pod.Containers[0]
	if !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("plugin container should have a read-only root and no privilege escalation")
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[*e.Name] = *e.Value
	}
	for k, want := range map[string]string{
		"OIKADE_MQTT_URL":     "mqtt://broker:1883",
		"OIKADE_TOPIC":        "oikade/v1/home/sonoff",
		"OIKADE_DEVICES_FILE": "/etc/oikade/config/devices.json",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["OIKADE_SECRETS_DIR"]; ok || len(pod.Volumes) != 2 {
		t.Errorf("no secretRef should mean no secrets mount: env %v, volumes %d", env, len(pod.Volumes))
	}

	p := plugin()
	p.Spec.HostNetwork = true
	p.Spec.SecretRef = &corev1.LocalObjectReference{Name: "sonoff-lan-keys"}
	tmpl = PluginDeployment(PluginInput{
		Plugin: p, MQTTURL: "mqtt://broker:1883", ConfigHash: "h", SecretVersion: "42",
	}).Spec.Template
	if tmpl.Annotations[AnnotationSecretVersion] != "42" {
		t.Errorf("secret version annotation = %q", tmpl.Annotations[AnnotationSecretVersion])
	}
	pod = tmpl.Spec
	if !*pod.HostNetwork || *pod.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Error("hostNetwork should set ClusterFirstWithHostNet")
	}
	v := pod.Volumes[len(pod.Volumes)-1]
	if *v.Secret.SecretName != "sonoff-lan-keys" || !*v.Secret.Optional {
		t.Errorf("user secret should be mounted optional: %+v", v.Secret)
	}
	m := pod.Containers[0].VolumeMounts[len(pod.Containers[0].VolumeMounts)-1]
	if *m.MountPath != "/etc/oikade/secrets" || !*m.ReadOnly {
		t.Errorf("user secret mount = %+v", m)
	}
}

// TestPluginConfigUnchanged pins devices.json to its bytes: a change rehashes the config, so every
// plugin pod restarts on an operator upgrade.
func TestPluginConfigUnchanged(t *testing.T) {
	for _, tc := range []struct {
		golden  string
		devices []contractv1.DeclaredDevice
	}{
		{"testdata/plugin-devices-empty.json", nil},
		{"testdata/plugin-devices.json", []contractv1.DeclaredDevice{
			{ID: "1000aabbcc", Name: "porch-light", DisplayName: "Porch light", Room: "porch",
				Parameters: map[string]string{"model": "MINIR4", "capability": "light.on"}},
			{ID: "1000ddeeff", Name: "fan", DisplayName: "Fan \"<big>\" & co"},
			{ID: "1000ffeedd", Name: "heater", DisplayName: "Heater", Parameters: map[string]string{}},
		}},
	} {
		out, err := PluginConfig(tc.devices)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(tc.golden)
		if err != nil {
			t.Fatal(err)
		}
		// An editor may add a final newline; the bytes before it are what matter.
		if !bytes.Equal(out, bytes.TrimSuffix(want, []byte("\n"))) {
			t.Errorf("devices.json changed:\n got %s\nwant %s", out, want)
		}
	}
}
