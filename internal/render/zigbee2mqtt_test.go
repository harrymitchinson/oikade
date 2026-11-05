package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		// Device names live in devices.yaml, which Zigbee2MQTT owns.
		"devices": "devices.yaml",
	} {
		if got := get(path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if key := get("advanced.network_key").([]any); len(key) != 16 || key[2] != 3.0 {
		t.Errorf("network_key = %v", key)
	}
	if strings.Contains(string(out), "friendly_name") {
		t.Errorf("configuration.yaml names devices:\n%s", out)
	}
}

func TestZigbeeDevicesSeed(t *testing.T) {
	out, err := ZigbeeDevicesSeed([]ZigbeeDevice{
		{IEEEAddress: "0x000b57fffe000001", FriendlyName: "porch-light"},
		{IEEEAddress: "0x00158d0000000003", FriendlyName: "garage-door"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]map[string]string
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["0x000b57fffe000001"]["friendly_name"] != "porch-light" || got["0x00158d0000000003"]["friendly_name"] != "garage-door" {
		t.Errorf("seed = %v", got)
	}
	// No declared devices is an empty map, which Zigbee2MQTT reads as no device settings.
	if empty, _ := ZigbeeDevicesSeed(nil); strings.TrimSpace(string(empty)) != "{}" {
		t.Errorf("empty seed = %q", empty)
	}
}

// TestZigbeeInitContainer runs the init container's script against a temporary data directory.
func TestZigbeeInitContainer(t *testing.T) {
	init := ZigbeeDeployment(zigbeeInstance("tcp://slzb:6638"), "img", "h").Spec.Template.Spec.InitContainers[0]
	if !slices.Equal(init.Command, []string{"sh", "-c", zigbeeInitScript("/config", zigbeeDataPath)}) {
		t.Fatalf("init command = %v", init.Command)
	}
	root := t.TempDir()
	config, data := filepath.Join(root, "config"), filepath.Join(root, "data")
	for _, dir := range []string{config, data} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	script := zigbeeInitScript(config, data)
	run := func() {
		t.Helper()
		if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
	}

	write(filepath.Join(config, "configuration.yaml"), "rendered: 1\n")
	write(filepath.Join(config, "devices.yaml"), "seed: 1\n")
	run()
	if got := read(filepath.Join(data, "devices.yaml")); got != "seed: 1\n" {
		t.Errorf("devices.yaml on a new PVC = %q, want the seed", got)
	}

	// Renamed in the frontend, then restarted with a new config and a new seed.
	write(filepath.Join(data, "devices.yaml"), "renamed: 1\n")
	write(filepath.Join(config, "configuration.yaml"), "rendered: 2\n")
	write(filepath.Join(config, "devices.yaml"), "seed: 2\n")
	run()
	if got := read(filepath.Join(data, "configuration.yaml")); got != "rendered: 2\n" {
		t.Errorf("configuration.yaml = %q, want the rendered one", got)
	}
	if got := read(filepath.Join(data, "devices.yaml")); got != "renamed: 1\n" {
		t.Errorf("devices.yaml after a restart = %q, want the frontend's", got)
	}

	// An empty file, as left by a crash mid-write, is seeded again.
	write(filepath.Join(data, "devices.yaml"), "")
	run()
	if got := read(filepath.Join(data, "devices.yaml")); got != "seed: 2\n" {
		t.Errorf("empty devices.yaml = %q after a restart, want the seed", got)
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
