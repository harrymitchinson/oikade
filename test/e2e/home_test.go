//go:build e2e

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

	"github.com/harrymitchinson/oikade/internal/sun"
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
  triggers: [{change: {device: hallway-motion, capability: sensor.occupancy-detected}}]
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
  triggers: [{change: {room: porch, capability: sensor.contact-open}}]
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
  triggers: [{change: {device: front-door, capability: sensor.contact-open}}]
  do:
  - set: {device: porch-light, capability: light.on, value: false}
`

// readmeNamespace is where the README's Rules examples are applied, apart from the home the
// other tests drive.
const readmeNamespace = "e2e-readme"

// controlManifests adds two plugin plugs: a fan held on all day by a window hold, and a towel rail
// that arbitrates switch.on, with no Rule claiming it.
const controlManifests = `
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: fan-plug}
spec:
  plugin: {pluginRef: {name: example-switch}, id: fan-relay, parameters: {capability: switch.on}}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: fan-all-day}
spec:
  while: {window: {start: "00:00", end: "00:00"}}
  hold: {device: fan-plug, capability: switch.on, value: true}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: rail-plug}
spec:
  plugin: {pluginRef: {name: example-switch}, id: rail-relay, parameters: {capability: switch.on}}
  control:
  - {capability: switch.on, default: false, resolve: PreferOn, external: Override, overrideHold: 1h}
`

// budgetDevice and budgetRule add a plugin plug run by a budget hold. Its budget is the whole day,
// so every slot left today is planned, and it runs from now whatever time the test runs.
const budgetDevice = `
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: dryer-plug}
spec:
  plugin: {pluginRef: {name: example-switch}, id: dryer-relay, parameters: {capability: switch.on}}
  control:
  - {capability: switch.on, default: false, resolve: PreferOn, external: Override, overrideHold: 1h}
`

const budgetRule = `
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: dryer-daily}
spec:
  while: {budget: {minutes: {expression: "1440"}}}
  hold: {device: dryer-plug, capability: switch.on, value: true}
`

// runtimeTimezone and runtimeSite are the runtime's --timezone, --latitude and --longitude. The zone
// isn't UTC, so a Rule that used UTC instead would fail.
const runtimeTimezone = "Australia/Sydney"

var runtimeSite = sun.Site{Latitude: -33.8688, Longitude: 151.2093}

// timetableManifests turns a plug off at a time of day on the given days, and on at the same time
// on the other days.
func timetableManifests(at time.Time) string {
	today := at.Weekday().String()[:3]
	var others []string
	for d := range 7 {
		if day := time.Weekday(d).String()[:3]; day != today {
			others = append(others, day)
		}
	}
	return fmt.Sprintf(`
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: downstairs-plug}
spec:
  plugin: {pluginRef: {name: example-switch}, id: downstairs-relay, parameters: {capability: switch.on}}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: downstairs-off}
spec:
  triggers: [{at: {time: "%[1]s", days: [%[2]s]}}]
  do:
  - set: {device: downstairs-plug, capability: switch.on, value: false}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: downstairs-on-other-days}
spec:
  triggers: [{at: {time: "%[1]s", days: [%[3]s]}}]
  do:
  - set: {device: downstairs-plug, capability: switch.on, value: true}
`, at.Format("15:04"), today, strings.Join(others, ", "))
}

// sunsetManifests turns the porch light on 15 minutes before sunset, and holds it on from an hour
// to two hours after.
const sunsetManifests = `
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: porch-dusk}
spec:
  triggers: [{sun: {event: Sunset, offset: -15m}}]
  do:
  - set: {device: porch-light, capability: light.on, value: true}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: porch-evening}
spec:
  while: {window: {start: {sun: Sunset, offset: 1h}, end: {sun: Sunset, offset: 2h}}}
  hold: {device: porch-light, capability: light.on, value: true}
`

// nextSunset returns the first sunset after now, moved by each offset in turn, in the runtime's
// zone: the times a sun trigger or sun window edge next fires.
func nextSunset(now time.Time, offsets ...time.Duration) time.Time {
	loc, err := time.LoadLocation(runtimeTimezone)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var next time.Time
	local := now.In(loc)
	for i := -2; i <= 2; i++ {
		day := local.AddDate(0, 0, i)
		set, ok := runtimeSite.Time(sun.Sunset, day.Year(), day.Month(), day.Day(), loc)
		ExpectWithOffset(1, ok).To(BeTrue())
		for _, off := range offsets {
			if t := set.Add(off); t.After(now) && (next.IsZero() || t.Before(next)) {
				next = t
			}
		}
	}
	return next
}

// statusTime parses a time jsonpath read from a status, or returns the zero time.
func statusTime(s string, err error) time.Time {
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// bathroomManifests runs a fan while a humidity sensor reads over 70, and for a minute after.
const bathroomManifests = `
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: bathroom-sensor}
spec:
  plugin: {pluginRef: {name: example-switch}, id: bathroom-humidity, parameters: {capability: sensor.relative-humidity}}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: bathroom-fan}
spec:
  plugin: {pluginRef: {name: example-switch}, id: bathroom-fan-relay, parameters: {capability: switch.on}}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: bathroom-humid}
spec:
  while:
    when: devices["bathroom-sensor"]["sensor.relative-humidity"] > 70
    offDelay: 1m
  hold: {device: bathroom-fan, capability: switch.on, value: true}
`

// heaterManifests holds a water heater on in a window from an hour before from to two hours after.
// The heater has no control entry, so the last write wins.
func heaterManifests(from time.Time) string {
	return fmt.Sprintf(`
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: water-heater}
spec:
  plugin: {pluginRef: {name: example-switch}, id: heater-relay, parameters: {capability: switch.on}}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: water-heater-now}
spec:
  while: {window: {start: "%s", end: "%s"}}
  hold: {device: water-heater, capability: switch.on, value: true}
`, from.Add(-time.Hour).Format("15:04"), from.Add(2*time.Hour).Format("15:04"))
}

// pluginDevices is the plugin's devices topic with every relay found. It clears the device error
// that "serves a plugin Device fed over MQTT" reports, so it's published only after that test.
const pluginDevices = `{"devices":[` +
	`{"id":"porch-relay","found":true,"capabilities":["light.on"]},` +
	`{"id":"fan-relay","found":true,"capabilities":["switch.on"]},` +
	`{"id":"rail-relay","found":true,"capabilities":["switch.on"]},` +
	`{"id":"dryer-relay","found":true,"capabilities":["switch.on"]},` +
	`{"id":"downstairs-relay","found":true,"capabilities":["switch.on"]},` +
	`{"id":"bathroom-humidity","found":true,"capabilities":["sensor.relative-humidity"]},` +
	`{"id":"bathroom-fan-relay","found":true,"capabilities":["switch.on"]},` +
	`{"id":"heater-relay","found":true,"capabilities":["switch.on"]}],"undeclared":0}`

// timetableDevices is the plugin's devices topic with the porch and downstairs relays found. The
// timetable needs the downstairs relay before "drives a plug from a window hold" announces the fan.
const timetableDevices = `{"devices":[` +
	`{"id":"porch-relay","found":true,"capabilities":["light.on"]},` +
	`{"id":"downstairs-relay","found":true,"capabilities":["switch.on"]}],"undeclared":0}`

// readmeManifests returns every YAML block in the README's Rules section that is a complete
// manifest, as one stream.
func readmeManifests() string {
	root, err := utils.GetProjectDir()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	raw, err := os.ReadFile(filepath.Join(root, "README.md"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	readme := string(raw)
	start := strings.Index(readme, "\n## Rules\n")
	end := strings.Index(readme, "\n## Capabilities\n")
	ExpectWithOffset(1, start).To(BeNumerically(">=", 0))
	ExpectWithOffset(1, end).To(BeNumerically(">", start))
	var docs []string
	for _, m := range regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(readme[start:end], -1) {
		if strings.HasPrefix(m[1], "apiVersion:") {
			docs = append(docs, m[1])
		}
	}
	return strings.Join(docs, "---\n")
}

// listen starts listening on topic for d, and returns a func that waits that long and returns
// whatever came, one "topic payload" per line. Unlike subscribe, it expects nothing.
func listen(topic string, d time.Duration) func() string {
	var out bytes.Buffer
	sub := exec.Command("kubectl", "-n", homeNamespace, "exec", mqttClient, "--",
		"mosquitto_sub", "-h", mqttHost, "-t", topic, "-v", "-W", fmt.Sprint(int(d.Seconds())))
	sub.Stdout = &out
	ExpectWithOffset(1, sub.Start()).To(Succeed())
	time.Sleep(3 * time.Second) // let the subscription land before publishing
	return func() string {
		_ = sub.Wait() // mosquitto_sub fails when -W runs out
		return out.String()
	}
}

// subscribe starts listening for count messages on topic, and returns a func that waits for them
// and returns them, one "topic payload" per line. It gives up after 90s.
func subscribe(topic string, count int) func() string {
	return subscribeFor(topic, count, 90*time.Second)
}

// subscribeFor is subscribe, giving up after timeout.
func subscribeFor(topic string, count int, timeout time.Duration) func() string {
	var out bytes.Buffer
	sub := exec.Command("kubectl", "-n", homeNamespace, "exec", mqttClient, "--",
		"mosquitto_sub", "-h", mqttHost, "-t", topic, "-v", "-C", fmt.Sprint(count),
		"-W", fmt.Sprint(int(timeout.Seconds())))
	sub.Stdout = &out
	ExpectWithOffset(1, sub.Start()).To(Succeed())
	time.Sleep(3 * time.Second) // let the subscription land before publishing
	return func() string {
		ExpectWithOffset(1, sub.Wait()).To(Succeed())
		return out.String()
	}
}

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
		// The timetable's fire time, and its write to the plug, which a later test waits for.
		var timetableAt time.Time
		var timetableWrites func() string

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
			subs := []string{"status", "devices"}
			for _, id := range []string{"porch-relay", "fan-relay", "rail-relay", "dryer-relay", "downstairs-relay",
				"bathroom-humidity", "bathroom-fan-relay", "heater-relay"} {
				subs = append(subs, id+"/availability", id+"/state")
			}
			for _, sub := range subs {
				_, _ = mqttExec("mosquitto_pub", "-h", mqttHost, "-q", "1", "-r", "-n", "-t", pluginRoot+"/"+sub)
			}
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", homeNamespace, readmeNamespace, "--wait=false"))
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			for _, args := range [][]string{
				{"-n", namespace, "logs", "deploy/oikade-runtime"},
				{"-n", homeNamespace, "logs", "deploy/main-hap"},
				{"-n", homeNamespace, "get", "devices,rules,bridges,zigbee2mqtts,plugins,pods", "-o", "wide"},
				{"-n", readmeNamespace, "get", "devices,rules", "-o", "yaml"},
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
			wait := subscribe("z2m/+/set", 1)

			By("reporting motion")
			publish("z2m/hallway-motion", `{"occupancy":true,"temperature":19.5}`, false)
			Expect(wait()).To(ContainSubstring(`z2m/0x000b57fffe000001/set {"state":"ON"}`))

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

		It("logs the build version on startup in every role", func() {
			for _, args := range [][]string{
				{"-n", namespace, "logs", "deploy/oikade-controller-manager"},
				{"-n", namespace, "logs", "deploy/oikade-runtime"},
				{"-n", homeNamespace, "logs", "deploy/main-hap"},
			} {
				Eventually(func(g Gomega) {
					out, err := utils.Run(exec.Command("kubectl", args...))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(ContainSubstring(`"version":"`+managerVersion+`"`), "kubectl %s", strings.Join(args, " "))
				}).Should(Succeed())
			}
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
			wait := subscribe("z2m/+/set", 2)

			By("opening the porch door")
			publish("z2m/front-door", `{"contact":false}`, false)
			out := wait()
			Expect(out).To(ContainSubstring(`z2m/0x000b57fffe000001/set {"state":"OFF"}`))
			Expect(out).To(ContainSubstring("z2m/" + secondLamp + `/set {"state":"OFF"}`))
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
			wait := subscribe(pluginRoot+"/porch-relay/set", 1)
			// The test above left the door open; closing it is a change.
			publish("z2m/front-door", `{"contact":true}`, false)
			Expect(wait()).To(ContainSubstring(pluginRoot + `/porch-relay/set {"light.on":false}`))

			By("reporting a device error")
			publish(pluginRoot+"/devices", `{"devices":[{"id":"porch-relay","found":false,`+
				`"error":"no LAN key for porch-relay"}],"undeclared":0}`, true)
			Eventually(jsonpath("device", "porch-light", `{.status.conditions[?(@.type=="Resolved")].reason}`)).
				Should(Equal("PluginError"))
			Expect(jsonpath("device", "porch-light", `{.status.conditions[?(@.type=="Resolved")].message}`)()).
				To(Equal("no LAN key for porch-relay"))
		})

		// This needs the plugin from the test above. Kind can't fake the clock, so the timetable fires
		// at the first minute at least 45s from now, and "runs a timetable on the days it names"
		// checks it fired after the tests in between have run. It listens from before the Rules
		// exist: nothing else writes to the downstairs relay, so the one write it reads is theirs.
		It("schedules a timetable on the days it names", func() {
			loc, err := time.LoadLocation(runtimeTimezone)
			Expect(err).NotTo(HaveOccurred())
			publish(pluginRoot+"/devices", timetableDevices, true)
			publish(pluginRoot+"/downstairs-relay/availability", "online", true)
			publish(pluginRoot+"/downstairs-relay/state", `{"switch.on":true}`, true)
			timetableAt = time.Now().In(loc).Add(105 * time.Second).Truncate(time.Minute)
			timetableWrites = subscribeFor(pluginRoot+"/downstairs-relay/set", 1, time.Until(timetableAt)+time.Minute)
			apply(timetableManifests(timetableAt))
			for _, rule := range []string{"downstairs-off", "downstairs-on-other-days"} {
				Eventually(jsonpath("rule", rule, `{.status.conditions[?(@.type=="Ready")].status}`)).Should(Equal("True"))
				Eventually(jsonpath("rule", rule, `{.status.conditions[?(@.type=="Scheduled")].status}`)).
					Should(Equal("True"))
			}
			at := timetableAt
			Expect(statusTime(jsonpath("rule", "downstairs-off", "{.status.nextFire}")())).To(BeTemporally("==", at))
			tomorrow := time.Date(at.Year(), at.Month(), at.Day()+1, at.Hour(), at.Minute(), 0, 0, loc)
			Expect(statusTime(jsonpath("rule", "downstairs-on-other-days", "{.status.nextFire}")())).
				To(BeTemporally("==", tomorrow))
		})

		// This needs the plugin from the tests above, and the fan not yet announced on the devices topic.
		It("drives a plug from a window hold", func() {
			apply(controlManifests)
			Eventually(jsonpath("rule", "fan-all-day", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))

			// The hold writes once the plugin has announced the fan, so listen first.
			wait := subscribe(pluginRoot+"/fan-relay/set", 1)
			publish(pluginRoot+"/devices", pluginDevices, true)
			publish(pluginRoot+"/fan-relay/availability", "online", true)
			publish(pluginRoot+"/fan-relay/state", `{"switch.on":false}`, true)
			Expect(wait()).To(ContainSubstring(pluginRoot + `/fan-relay/set {"switch.on":true}`))
			Eventually(jsonpath("rule", "fan-all-day", "{.status.active}")).Should(Equal("true"))
			Expect(jsonpath("rule", "fan-all-day", "{.status.nextChange}")()).NotTo(BeEmpty())
		})

		It("shows an outside change on an arbitrated plug as an Override claim", func() {
			publish(pluginRoot+"/rail-relay/availability", "online", true)
			publish(pluginRoot+"/rail-relay/state", `{"switch.on":false}`, true)
			Eventually(jsonpath("device", "rail-plug", "{.status.control[0].level}")).Should(Equal("Default"))

			By("switching the rail on at the plug")
			publish(pluginRoot+"/rail-relay/state", `{"switch.on":true}`, true)
			Eventually(jsonpath("device", "rail-plug", "{.status.control[0].level}")).Should(Equal("Override"))
			Expect(jsonpath("device", "rail-plug",
				`{.status.control[0].claims[?(@.winner==true)].source}`)()).To(Equal("Device"))
			Expect(jsonpath("device", "rail-plug", `{.status.control[0].claims[?(@.winner==true)].expires}`)()).NotTo(BeEmpty())
		})

		// This needs the plugin and its devices from the tests above.
		It("plans a budget and drives a plug from it", func() {
			// The plug is declared, and reports it's at its default, before the budget claims it, so
			// the only write after that is the budget's.
			publish(pluginRoot+"/dryer-relay/availability", "online", true)
			publish(pluginRoot+"/dryer-relay/state", `{"switch.on":false}`, true)
			apply(budgetDevice)
			Eventually(jsonpath("device", "dryer-plug", "{.status.control[0].level}")).Should(Equal("Default"))
			wait := subscribe(pluginRoot+"/dryer-relay/set", 1)
			apply(budgetRule)
			Eventually(jsonpath("rule", "dryer-daily", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("rule", "dryer-daily", `{.status.conditions[?(@.type=="Planned")].status}`)).
				Should(Equal("True"))
			Expect(jsonpath("rule", "dryer-daily", "{.status.today.targetSource} {.status.today.targetMinutes}")()).
				To(Equal("Expression 1440"))
			Expect(jsonpath("rule", "dryer-daily", "{.status.today.blocks[0].start}")()).NotTo(BeEmpty())
			Expect(wait()).To(ContainSubstring(pluginRoot + `/dryer-relay/set {"switch.on":true}`))
			Eventually(jsonpath("device", "dryer-plug", `{.status.control[0].claims[?(@.winner==true)].source}`)).
				Should(Equal("Rule/dryer-daily"))
			Eventually(jsonpath("rule", "dryer-daily", "{.status.today.starts}")).Should(Equal("1"))
		})

		// This needs the porch light from the tests above.
		It("schedules a porch light from sunset", func() {
			now := time.Now()
			apply(sunsetManifests)
			Eventually(jsonpath("rule", "porch-dusk", `{.status.conditions[?(@.type=="Scheduled")].status}`)).
				Should(Equal("True"))
			Eventually(func() time.Time { return statusTime(jsonpath("rule", "porch-dusk", "{.status.nextFire}")()) }).
				Should(BeTemporally("==", nextSunset(now, -15*time.Minute)))
			Eventually(jsonpath("rule", "porch-evening", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))
			Eventually(func() time.Time { return statusTime(jsonpath("rule", "porch-evening", "{.status.nextChange}")()) }).
				Should(BeTemporally("==", nextSunset(now, time.Hour, 2*time.Hour)))
			// A hold reports Scheduled only when its sun times can't be computed.
			Expect(jsonpath("rule", "porch-evening", `{.status.conditions[?(@.type=="Scheduled")].status}`)()).
				To(BeEmpty())
		})

		// This needs the plugin and its devices from the tests above.
		It("runs a bathroom fan while it's humid, and for its offDelay after", func() {
			publish(pluginRoot+"/bathroom-humidity/availability", "online", true)
			publish(pluginRoot+"/bathroom-humidity/state", `{"sensor.relative-humidity":55}`, true)
			publish(pluginRoot+"/bathroom-fan-relay/availability", "online", true)
			publish(pluginRoot+"/bathroom-fan-relay/state", `{"switch.on":false}`, true)
			apply(bathroomManifests)
			Eventually(jsonpath("rule", "bathroom-humid", `{.status.conditions[?(@.type=="Ready")].status}`)).
				Should(Equal("True"))
			Eventually(jsonpath("device", "bathroom-sensor", `{.status.conditions[?(@.type=="Available")].status}`)).
				Should(Equal("True"))
			Expect(jsonpath("rule", "bathroom-humid", "{.status.active}")()).NotTo(Equal("true"))

			By("someone running a shower")
			wait := subscribe(pluginRoot+"/bathroom-fan-relay/set", 1)
			publish(pluginRoot+"/bathroom-humidity/state", `{"sensor.relative-humidity":78}`, true)
			Expect(wait()).To(ContainSubstring(pluginRoot + `/bathroom-fan-relay/set {"switch.on":true}`))
			publish(pluginRoot+"/bathroom-fan-relay/state", `{"switch.on":true}`, true)
			Eventually(jsonpath("rule", "bathroom-humid", "{.status.active}")).Should(Equal("true"))

			By("the humidity dropping")
			wait = subscribe(pluginRoot+"/bathroom-fan-relay/set", 1)
			dropped := time.Now()
			publish(pluginRoot+"/bathroom-humidity/state", `{"sensor.relative-humidity":62}`, true)
			Expect(wait()).To(ContainSubstring(pluginRoot + `/bathroom-fan-relay/set {"switch.on":false}`))
			Expect(time.Since(dropped)).To(BeNumerically(">=", 55*time.Second), "the fan ran on for its offDelay")
			publish(pluginRoot+"/bathroom-fan-relay/state", `{"switch.on":false}`, true)
			Eventually(jsonpath("rule", "bathroom-humid", "{.status.active}")).Should(Equal("false"))
		})

		It("applies the README's Rules examples as written", func() {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", readmeNamespace))
			Expect(err).NotTo(HaveOccurred())
			cmd := exec.Command("kubectl", "-n", readmeNamespace, "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(readmeManifests())
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("rule.home.oikade.dev/garage-overheat"), "the README examples weren't found")

			for _, kind := range []string{"rules", "devices"} {
				Eventually(func(g Gomega) {
					out, err := utils.Run(exec.Command("kubectl", "-n", readmeNamespace, "get", kind, "-o",
						`jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status} {end}`))
					g.Expect(err).NotTo(HaveOccurred())
					for _, entry := range strings.Fields(out) {
						g.Expect(entry).To(HaveSuffix("=True"))
					}
					g.Expect(strings.Fields(out)).NotTo(BeEmpty())
				}).Should(Succeed(), "every %s in the README is Ready", kind)
			}
		})

		// This checks the timetable "schedules a timetable on the days it names" applied.
		It("runs a timetable on the days it names", func() {
			at, loc := timetableAt, timetableAt.Location()
			By("waiting for " + at.Format("15:04") + " in " + runtimeTimezone)
			Expect(timetableWrites()).To(ContainSubstring(pluginRoot + `/downstairs-relay/set {"switch.on":false}`))
			Eventually(func() time.Time {
				return statusTime(jsonpath("rule", "downstairs-off", "{.status.lastFire}")())
			}).Should(BeTemporally("==", at))
			Expect(statusTime(jsonpath("rule", "downstairs-off", "{.status.nextFire}")())).
				To(BeTemporally("==", time.Date(at.Year(), at.Month(), at.Day()+7, at.Hour(), at.Minute(), 0, 0, loc)))
			Consistently(jsonpath("rule", "downstairs-on-other-days", "{.status.lastFire}"), 10*time.Second).
				Should(BeEmpty())
		})

		// This needs the holds, claims and time triggers from the tests above.
		It("serves the Rule metrics the dashboard queries", func() {
			root, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			dashboard, err := os.ReadFile(filepath.Join(root, "config", "grafana", "oikade-home.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(dashboard)).NotTo(ContainSubstring("oikade_automation_"))
			queried := regexp.MustCompile(`oikade_(rule|device_control)_[a-z_]+`).FindAllString(string(dashboard), -1)
			Expect(queried).NotTo(BeEmpty())
			out, err := mqttExec("wget", "-qO-", runtimeURL)
			Expect(err).NotTo(HaveOccurred())
			for _, name := range queried {
				Expect(out).To(MatchRegexp(`(?m)^`+name+`[{ ]`), "the dashboard queries %s", name)
			}
		})

		// This needs the plugin and its devices from the tests above. It restarts the runtime, which
		// drops the counters the dashboard test reads until they count again, so it runs last.
		It("keeps a Home app tap on a held water heater across a runtime restart", func() {
			loc, err := time.LoadLocation(runtimeTimezone)
			Expect(err).NotTo(HaveOccurred())
			publish(pluginRoot+"/heater-relay/availability", "online", true)
			publish(pluginRoot+"/heater-relay/state", `{"switch.on":false}`, true)
			wait := subscribe(pluginRoot+"/heater-relay/set", 1)
			apply(heaterManifests(time.Now().In(loc)))
			Expect(wait()).To(ContainSubstring(pluginRoot + `/heater-relay/set {"switch.on":true}`))
			publish(pluginRoot+"/heater-relay/state", `{"switch.on":true}`, true)
			Eventually(jsonpath("rule", "water-heater-now", "{.status.active}")).Should(Equal("true"))

			By("turning the heater off in the Home app")
			// The bridge announces the write, then the plug reports it.
			_, err = mqttExec("mosquitto_pub", "-h", mqttHost, "-q", "1", "-t", "oikade/v1/_homekit/"+homeNamespace+"/main",
				"-m", `{"bridge":"main","device":"water-heater","capability":"switch.on","value":false}`)
			Expect(err).NotTo(HaveOccurred())
			publish(pluginRoot+"/heater-relay/state", `{"switch.on":false}`, true)
			Eventually(func(g Gomega) {
				out, err := mqttExec("wget", "-qO-", runtimeURL)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(MatchRegexp(`oikade_capability_value\{capability="switch.on",` +
					`device="water-heater",namespace="e2e-home"[^}]*\} 0\n`))
			}).Should(Succeed())

			By("restarting the runtime mid-window")
			heard := listen(pluginRoot+"/heater-relay/set", 100*time.Second)
			_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "restart", "deployment/oikade-runtime"))
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "status", "deployment/oikade-runtime",
				"--timeout=3m"))
			Expect(err).NotTo(HaveOccurred())
			Expect(heard()).To(BeEmpty(), "the restarted runtime wrote to the heater")
			Expect(jsonpath("rule", "water-heater-now", "{.status.active}")()).To(Equal("true"))
		})
	})
}
