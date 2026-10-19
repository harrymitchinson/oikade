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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
)

func zigbeeInstance(port string) *zigbeev1alpha1.Zigbee2MQTT {
	z := &zigbeev1alpha1.Zigbee2MQTT{}
	z.Name, z.Namespace = "z2m", "home"
	z.Spec.Serial = zigbeev1alpha1.SerialSpec{Port: port, Adapter: "ember"}
	return z
}

func TestZigbeeNetworkRoundTrip(t *testing.T) {
	n, err := NewZigbeeNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if n.PanID == 0 || n.PanID == 0xFFFF {
		t.Fatalf("reserved PAN ID %d", n.PanID)
	}
	got, err := ParseZigbeeNetwork(ZigbeeNetworkSecret(zigbeeInstance("tcp://x:6638"), n).Data)
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("round trip: got %+v, want %+v", got, n)
	}
	if _, err := ParseZigbeeNetwork(map[string][]byte{NetworkKeyKey: []byte("abcd")}); err == nil {
		t.Fatal("short network key accepted")
	}
}

func TestZigbeeConfig(t *testing.T) {
	z := zigbeeInstance("tcp://slzb:6638")
	z.Spec.BaseTopic = "zigbee"
	n := ZigbeeNetwork{Key: [16]byte{1, 2, 3}, PanID: 6754, ExtendedPanID: [8]byte{9}}
	out, err := ZigbeeConfig(ZigbeeConfigInput{
		Instance: z, Network: n, MQTTURL: "mqtt://broker:1883",
		Devices: []ZigbeeDevice{{IEEEAddress: "0x000b57fffe000001", FriendlyName: "hallway-light"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	get := func(path string) any {
		var v any = cfg
		for k := range strings.SplitSeq(path, ".") {
			v = v.(map[string]any)[k]
		}
		return v
	}
	for path, want := range map[string]any{
		"mqtt.base_topic":       "zigbee",
		"mqtt.server":           "mqtt://broker:1883",
		"serial.port":           "tcp://slzb:6638",
		"serial.adapter":        "ember",
		"advanced.pan_id":       6754.0,
		"advanced.channel":      11.0,
		"frontend.enabled":      false,
		"homeassistant.enabled": false,
		"availability.enabled":  true,
	} {
		if got := get(path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if key := get("advanced.network_key").([]any); len(key) != 16 || key[2] != 3.0 {
		t.Errorf("network_key = %v", key)
	}
	if name := get("devices").(map[string]any)["0x000b57fffe000001"].(map[string]any)["friendly_name"]; name != "hallway-light" {
		t.Errorf("friendly_name = %v", name)
	}
}

func TestZigbeeDeploymentCoordinators(t *testing.T) {
	network := ZigbeeDeployment(zigbeeInstance("tcp://slzb:6638"), "img", "h").Spec.Template.Spec
	if *network.SecurityContext.RunAsNonRoot != true || len(network.Volumes) != 2 {
		t.Errorf("network coordinator pod should be non-root with 2 volumes: %+v", network)
	}
	if network.Containers[0].SecurityContext == nil || *network.Containers[0].SecurityContext.AllowPrivilegeEscalation {
		t.Error("network coordinator container should drop privileges")
	}

	usb := ZigbeeDeployment(zigbeeInstance("/dev/ttyUSB0"), "img", "h").Spec.Template.Spec
	if len(usb.Volumes) != 3 || usb.Volumes[2].HostPath == nil || *usb.Volumes[2].HostPath.Path != "/dev/ttyUSB0" ||
		*usb.Volumes[2].HostPath.Type != corev1.HostPathCharDev {
		t.Errorf("USB coordinator needs a hostPath char device: %+v", usb.Volumes)
	}
	c := usb.Containers[0]
	if c.SecurityContext == nil || !*c.SecurityContext.Privileged || len(c.VolumeMounts) != 2 {
		t.Errorf("USB container must be privileged with the device mounted: %+v", c)
	}
}
