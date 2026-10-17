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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/harry/oikade/test/utils"
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
kind: Automation
metadata: {name: motion-light}
spec:
  triggers: [{device: hallway-motion, capability: sensor.occupancy-detected}]
  condition: trigger.value
  actions:
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
			cmd := exec.Command("kubectl", "-n", homeNamespace, "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(homeManifests)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", homeNamespace, "--wait=false"))
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			for _, args := range [][]string{
				{"-n", namespace, "logs", "deploy/oikade-runtime"},
				{"-n", homeNamespace, "logs", "deploy/main-hap"},
				{"-n", homeNamespace, "get", "devices,automations,bridges,zigbee2mqtts,pods", "-o", "wide"},
			} {
				out, _ := utils.Run(exec.Command("kubectl", args...))
				_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), out)
			}
		})

		It("renders Zigbee2MQTT config with friendly names from Git", func() {
			Eventually(func(g Gomega) {
				out, err := kubectl("get", "secret", "z2m-config", "-o", `jsonpath={.data.configuration\.yaml}`)
				g.Expect(err).NotTo(HaveOccurred())
				decoded, err := utils.Run(exec.Command("sh", "-c", "echo "+out+" | base64 -d"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(decoded).To(ContainSubstring("friendly_name: hallway-light"))
				g.Expect(decoded).To(ContainSubstring("server: mqtt://oikade-mosquitto.oikade-system.svc:1883"))
			}).Should(Succeed())
		})

		It("resolves Devices from the Zigbee2MQTT device list", func() {
			root, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			fixture := filepath.Join(root, "internal", "driver", "zigbee2mqtt", "testdata", "bridge-devices.json")
			devices, err := os.ReadFile(fixture)
			Expect(err).NotTo(HaveOccurred())
			publish("z2m/bridge/devices", string(bytes.Join(bytes.Fields(devices), []byte(" "))), true)
			publish("z2m/hallway-motion/availability", `{"state":"online"}`, true)

			Eventually(jsonpath("device", "hallway-light", `{.status.conditions[?(@.type=="Resolved")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("device", "hallway-light", `{.status.capabilities[*].name}`)).
				Should(Equal("light.on light.level link.quality"))
			Eventually(jsonpath("device", "hallway-motion", `{.status.conditions[?(@.type=="Available")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("automation", "motion-light", `{.status.conditions[?(@.type=="Ready")].status}`)).
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

			Eventually(jsonpath("automation", "motion-light", "{.status.lastTriggered}")).ShouldNot(BeEmpty())
		})

		It("exports device state to Prometheus", func() {
			Eventually(func(g Gomega) {
				out, err := mqttExec("wget", "-qO-", runtimeURL)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(`oikade_capability_value{capability="sensor.temperature",` +
					`device="hallway-motion",namespace="e2e-home",room="hallway"} 19.5`))
				g.Expect(out).To(ContainSubstring(
					`oikade_automation_runs_total{automation="motion-light",namespace="e2e-home",result="succeeded"} 1`))
				g.Expect(out).To(MatchRegexp(`oikade_undeclared_devices\{instance="z2m",namespace="e2e-home"\} 3`))
			}).Should(Succeed())
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
	})
}
