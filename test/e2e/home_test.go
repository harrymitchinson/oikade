//go:build e2e

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

package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harrymitchinson/oikade/test/utils"
)

const (
	homeNamespace = "e2e-home"
	mqttHost      = "oikade-mosquitto.oikade-system.svc"
	runtimeURL    = "http://oikade-runtime-metrics.oikade-system.svc:8080/metrics"
	// mqttClient runs mosquitto_pub/sub and wget; it stands in for Zigbee2MQTT.
	mqttClient = "mqtt-client"
)

// homeManifests declares a home whose Zigbee2MQTT has no coordinator: its pod is expected to
// crash, and the test plays Zigbee2MQTT on the bus instead.
const homeManifests = `
apiVersion: zigbee.oikade.dev/v1alpha1
kind: Zigbee2MQTT
metadata: {name: z2m}
spec:
  serial: {port: "tcp://no-coordinator.invalid:6638", adapter: zstack}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Room
metadata: {name: hallway}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: hallway-light}
spec:
  roomRef: {name: hallway}
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x000b57fffe000001"}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: hallway-motion}
spec:
  roomRef: {name: hallway}
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x00158d0000000002"}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: motion-light}
spec:
  on: [{change: {device: hallway-motion, capability: sensor.occupancy-detected}}]
  condition: trigger.value
  do:
  - set: {device: hallway-light, capability: light.on, value: true}
---
apiVersion: v1
kind: Secret
metadata: {name: homekit-pin}
stringData: {pin: "031-45-154"}
---
apiVersion: homekit.oikade.dev/v1alpha1
kind: Bridge
metadata: {name: main}
spec:
  pinSecretRef: {name: homekit-pin, key: pin}
`

// secondLamp is a copy of the fixture's IKEA lamp, so the hallway has two lights.
const secondLamp = "0x000b57fffe000006"

// roomManifests adds the second hallway lamp, puts the front door in a porch, and turns off every
// hallway light when the porch door opens. The porch Room is created later.
const roomManifests = `
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: hallway-lamp}
spec:
  roomRef: {name: hallway}
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "` + secondLamp + `"}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: front-door}
spec:
  roomRef: {name: porch}
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x00158d0000000003"}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: hallway-off}
spec:
  on: [{change: {room: porch, capability: sensor.contact-open}}]
  do:
  - set: {room: hallway, capability: light.on, value: false}
`

// pluginRoot is the plugin's oikade/v1 topic root.
const pluginRoot = "oikade/v1/" + homeNamespace + "/example-switch"

// pluginManifests declares a Plugin and a Device it serves. The pause image keeps the plugin pod
// running without speaking MQTT, so the test can play the plugin with mosquitto_pub.
const pluginManifests = `
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata: {name: example-switch}
spec:
  image: registry.k8s.io/pause:3.10
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: porch-light}
spec:
  roomRef: {name: porch}
  plugin:
    pluginRef: {name: example-switch}
    id: porch-relay
    parameters: {capability: light.on}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: porch-light-off}
spec:
  on: [{change: {device: front-door, capability: sensor.contact-open}}]
  do:
  - set: {device: porch-light, capability: light.on, value: false}
`

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", append([]string{"-n", homeNamespace}, args...)...))
}

func mqttExec(args ...string) (string, error) {
	return kubectl(append([]string{"exec", mqttClient, "--"}, args...)...)
}

func publish(topic, payload string, retain bool) {
	args := []string{"mosquitto_pub", "-h", mqttHost, "-q", "1", "-t", topic, "-m", payload}
	if retain {
		args = append(args, "-r")
	}
	_, err := mqttExec(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func apply(manifests string) {
	cmd := exec.Command("kubectl", "-n", homeNamespace, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// bridgeDevices returns the driver's bridge/devices fixture on one line, with secondLamp added
// when withSecondLamp is set.
func bridgeDevices(withSecondLamp bool) string {
	root, err := utils.GetProjectDir()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	raw, err := os.ReadFile(filepath.Join(root, "internal", "driver", "zigbee2mqtt", "testdata", "bridge-devices.json"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	if !withSecondLamp {
		return string(bytes.Join(bytes.Fields(raw), []byte(" ")))
	}
	var devices []map[string]any
	ExpectWithOffset(1, json.Unmarshal(raw, &devices)).To(Succeed())
	for _, d := range devices {
		if d["ieee_address"] == "0x000b57fffe000001" {
			lamp := maps.Clone(d)
			lamp["ieee_address"], lamp["friendly_name"] = secondLamp, secondLamp
			devices = append(devices, lamp)
			break
		}
	}
	out, err := json.Marshal(devices)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return string(out)
}

// secretKey returns one key of a Secret in the home namespace, decoded.
func secretKey(name, key string) (string, error) {
	out, err := kubectl("get", "secret", name, "-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(out)
	return string(decoded), err
}

func jsonpath(kind, name, path string) func() (string, error) {
	return func() (string, error) { return kubectl("get", kind, name, "-o", "jsonpath="+path) }
}

// homeScenario runs inside the ordered Manager container, after the controller is deployed.
func homeScenario() {
	Context("Home", Ordered, func() {
		BeforeAll(func() {
			By("creating the home namespace and an MQTT client pod")
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", homeNamespace))
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectl("run", mqttClient, "--image=eclipse-mosquitto:2.1.2-alpine", "--restart=Never",
				"--command", "--", "sleep", "3600")
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectl("wait", "--for=condition=Ready", "pod/"+mqttClient, "--timeout=3m")
			Expect(err).NotTo(HaveOccurred())

			By("declaring the home")
			apply(homeManifests)
		})

		AfterAll(func() {
			// The broker outlives the namespace; don't leave a fake plugin retained on it.
			for _, sub := range []string{"status", "devices", "porch-relay/availability", "porch-relay/state"} {
				_, _ = mqttExec("mosquitto_pub", "-h", mqttHost, "-q", "1", "-r", "-n", "-t", pluginRoot+"/"+sub)
			}
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", homeNamespace, "--wait=false"))
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			for _, args := range [][]string{
				{"-n", namespace, "logs", "deploy/oikade-runtime"},
				{"-n", homeNamespace, "logs", "deploy/main-hap"},
				{"-n", homeNamespace, "get", "devices,rules,bridges,zigbee2mqtts,plugins,pods", "-o", "wide"},
			} {
				out, _ := utils.Run(exec.Command("kubectl", args...))
				_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), out)
			}
		})

		It("renders Zigbee2MQTT config with friendly names from Git", func() {
			Eventually(func(g Gomega) {
				decoded, err := secretKey("z2m-config", "configuration.yaml")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(decoded).To(ContainSubstring("friendly_name: hallway-light"))
				g.Expect(decoded).To(ContainSubstring("server: mqtt://oikade-mosquitto.oikade-system.svc:1883"))
			}).Should(Succeed())
		})

		It("resolves Devices from the Zigbee2MQTT device list", func() {
			publish("z2m/bridge/devices", bridgeDevices(false), true)
			publish("z2m/hallway-motion/availability", `{"state":"online"}`, true)

			Eventually(jsonpath("device", "hallway-light", `{.status.conditions[?(@.type=="Resolved")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("device", "hallway-light", `{.status.capabilities[*].name}`)).
				Should(Equal("light.on light.level link.quality"))
			Eventually(jsonpath("device", "hallway-motion", `{.status.conditions[?(@.type=="Available")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("rule", "motion-light", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))
		})

		It("turns the light on when motion is detected", func() {
			By("listening for commands to Zigbee2MQTT")
			var out bytes.Buffer
			sub := exec.Command("kubectl", "-n", homeNamespace, "exec", mqttClient, "--",
				"mosquitto_sub", "-h", mqttHost, "-t", "z2m/+/set", "-v", "-C", "1", "-W", "90")
			sub.Stdout = &out
			Expect(sub.Start()).To(Succeed())
			time.Sleep(3 * time.Second) // let the subscription land before publishing

			By("reporting motion")
			publish("z2m/hallway-motion", `{"occupancy":true,"temperature":19.5}`, false)
			Expect(sub.Wait()).To(Succeed())
			Expect(out.String()).To(ContainSubstring(`z2m/0x000b57fffe000001/set {"state":"ON"}`))

			Eventually(jsonpath("rule", "motion-light", "{.status.lastTriggered}")).ShouldNot(BeEmpty())
		})

		It("exports device state to Prometheus", func() {
			Eventually(func(g Gomega) {
				out, err := mqttExec("wget", "-qO-", runtimeURL)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(`oikade_capability_value{capability="sensor.temperature",` +
					`device="hallway-motion",namespace="e2e-home",room="hallway"} 19.5`))
				g.Expect(out).To(ContainSubstring(
					`oikade_rule_runs_total{namespace="e2e-home",result="succeeded",rule="motion-light"} 1`))
				g.Expect(out).To(MatchRegexp(`oikade_undeclared_devices\{instance="z2m",namespace="e2e-home"\} 3`))
			}).Should(Succeed())
		})

		It("serves the Rule metrics the dashboard queries", func() {
			root, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			dashboard, err := os.ReadFile(filepath.Join(root, "config", "grafana", "oikade-home.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(dashboard)).NotTo(ContainSubstring("oikade_automation_"))
			queried := regexp.MustCompile(`oikade_rule_[a-z_]+`).FindAllString(string(dashboard), -1)
			Expect(queried).NotTo(BeEmpty())
			out, err := mqttExec("wget", "-qO-", runtimeURL)
			Expect(err).NotTo(HaveOccurred())
			for _, name := range queried {
				Expect(out).To(MatchRegexp(`(?m)^`+name+`[{ ]`), "the dashboard queries %s", name)
			}
		})

		It("has no Automation kind left", func() {
			_, err := utils.Run(exec.Command("kubectl", "get", "crd", "automations.home.oikade.dev"))
			Expect(err).To(MatchError(ContainSubstring("NotFound")))
		})

		It("runs a HomeKit bridge", func() {
			Eventually(jsonpath("bridge", "main", "{.status.accessories[*].device}")).
				Should(Equal("hallway-light hallway-motion"))
			Eventually(jsonpath("bridge", "main", `{.status.conditions[?(@.type=="Ready")].status}`), 4*time.Minute).
				Should(Equal("True"))
			Eventually(func(g Gomega) {
				out, err := kubectl("logs", "deploy/main-hap")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(`"accessories":2`))
			}).Should(Succeed())
		})

		// The rest run last: they declare more devices, which changes the bridge and undeclared counts.
		It("turns off every light in a Room with one action", func() {
			By("naming a Room that doesn't exist yet")
			apply(roomManifests)
			Eventually(jsonpath("rule", "hallway-off", `{.status.conditions[?(@.type=="Ready")].reason}`)).
				Should(Equal("RoomNotFound"))
			Expect(jsonpath("rule", "hallway-off", `{.status.conditions[?(@.type=="Ready")].message}`)()).
				To(Equal("Room(s) not found: porch"))

			By("creating the Room, without editing the Rule")
			apply("apiVersion: home.oikade.dev/v1alpha1\nkind: Room\nmetadata: {name: porch}\n")
			Eventually(jsonpath("rule", "hallway-off", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))

			By("pairing the second lamp")
			publish("z2m/bridge/devices", bridgeDevices(true), true)
			Eventually(jsonpath("device", "hallway-lamp", `{.status.capabilities[*].name}`)).
				Should(Equal("light.on light.level link.quality"))
			Eventually(jsonpath("device", "front-door", `{.status.conditions[?(@.type=="Resolved")].status}`)).
				Should(Equal("True"))

			By("listening for commands to Zigbee2MQTT")
			var out bytes.Buffer
			sub := exec.Command("kubectl", "-n", homeNamespace, "exec", mqttClient, "--",
				"mosquitto_sub", "-h", mqttHost, "-t", "z2m/+/set", "-v", "-C", "2", "-W", "90")
			sub.Stdout = &out
			Expect(sub.Start()).To(Succeed())
			time.Sleep(3 * time.Second) // let the subscription land before publishing

			By("opening the porch door")
			publish("z2m/front-door", `{"contact":false}`, false)
			Expect(sub.Wait()).To(Succeed())
			Expect(out.String()).To(ContainSubstring(`z2m/0x000b57fffe000001/set {"state":"OFF"}`))
			Expect(out.String()).To(ContainSubstring("z2m/" + secondLamp + `/set {"state":"OFF"}`))
		})

		// This needs the porch Room from the test above.
		It("serves a plugin Device fed over MQTT", func() {
			By("declaring a Plugin and a Device it serves")
			apply(pluginManifests)
			Eventually(func(g Gomega) {
				raw, err := secretKey("example-switch-plugin-config", "devices.json")
				g.Expect(err).NotTo(HaveOccurred())
				var cfg struct {
					Contract int `json:"contract"`
					Devices  []struct {
						ID         string            `json:"id"`
						Name       string            `json:"name"`
						Room       string            `json:"room"`
						Parameters map[string]string `json:"parameters"`
					} `json:"devices"`
				}
				g.Expect(json.Unmarshal([]byte(raw), &cfg)).To(Succeed())
				g.Expect(cfg.Contract).To(Equal(1))
				g.Expect(cfg.Devices).To(HaveLen(1))
				g.Expect(cfg.Devices[0].ID).To(Equal("porch-relay"))
				g.Expect(cfg.Devices[0].Name).To(Equal("porch-light"))
				g.Expect(cfg.Devices[0].Room).To(Equal("porch"))
				g.Expect(cfg.Devices[0].Parameters).To(Equal(map[string]string{"capability": "light.on"}))
			}).Should(Succeed())
			Eventually(jsonpath("plugin", "example-switch", `{.status.conditions[?(@.type=="Ready")].status}`), 3*time.Minute).
				Should(Equal("True"))
			Expect(jsonpath("plugin", "example-switch", "{.status.devices}")()).To(Equal("1"))

			By("playing the plugin on the bus")
			publish(pluginRoot+"/status", `{"state":"online","contract":1}`, true)
			publish(pluginRoot+"/devices", `{"devices":[{"id":"porch-relay","found":true,`+
				`"manufacturer":"Oikade","model":"Simulated switch","capabilities":["light.on"]}],"undeclared":0}`, true)
			publish(pluginRoot+"/porch-relay/availability", "online", true)
			publish(pluginRoot+"/porch-relay/state", `{"light.on":true}`, true)

			Eventually(jsonpath("device", "porch-light", `{.status.conditions[?(@.type=="Resolved")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("device", "porch-light", `{.status.capabilities[*].name}`)).Should(Equal("light.on"))
			Eventually(jsonpath("device", "porch-light", `{.status.conditions[?(@.type=="Available")].status}`)).
				Should(Equal("True"))
			Eventually(func(g Gomega) {
				out, err := mqttExec("wget", "-qO-", runtimeURL)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(`oikade_capability_value{capability="light.on",` +
					`device="porch-light",namespace="e2e-home",room="porch"} 1`))
				g.Expect(out).To(ContainSubstring(`oikade_plugin_up{namespace="e2e-home",plugin="example-switch"} 1`))
			}).Should(Succeed())

			By("switching the plugin Device from a Rule")
			Eventually(jsonpath("rule", "porch-light-off", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))
			var out bytes.Buffer
			sub := exec.Command("kubectl", "-n", homeNamespace, "exec", mqttClient, "--",
				"mosquitto_sub", "-h", mqttHost, "-t", pluginRoot+"/porch-relay/set", "-v", "-C", "1", "-W", "90")
			sub.Stdout = &out
			Expect(sub.Start()).To(Succeed())
			time.Sleep(3 * time.Second) // let the subscription land before publishing
			// The test above left the door open; closing it is a change.
			publish("z2m/front-door", `{"contact":true}`, false)
			Expect(sub.Wait()).To(Succeed())
			Expect(out.String()).To(ContainSubstring(pluginRoot + `/porch-relay/set {"light.on":false}`))

			By("reporting a device error")
			publish(pluginRoot+"/devices", `{"devices":[{"id":"porch-relay","found":false,`+
				`"error":"no LAN key for porch-relay"}],"undeclared":0}`, true)
			Eventually(jsonpath("device", "porch-light", `{.status.conditions[?(@.type=="Resolved")].reason}`)).
				Should(Equal("PluginError"))
			Expect(jsonpath("device", "porch-light", `{.status.conditions[?(@.type=="Resolved")].message}`)()).
				To(Equal("no LAN key for porch-relay"))
		})
	})
}
