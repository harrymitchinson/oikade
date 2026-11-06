// Package journeys follows a person through Oikade from outside: the real binary, a home made by
// oikade init, simulated devices, and for Matter a controller nobody here wrote. It runs on main and
// nightly (make test-journeys), not on PRs, because test/host covers each part faster.
package journeys

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
)

const waitLimit = 2 * time.Minute

// repo is this checkout, which a new home builds against so that nothing needs fetching.
func repo(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("couldn't find the repo")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// command runs a program, and fails the test if it fails.
func command(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// oikade builds the binary, for this machine or another one's env.
func oikade(t *testing.T, env ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oikade")
	command(t, repo(t), env, "go", "build", "-o", path, "./cmd/oikade")
	return path
}

// newHome makes a home with oikade init, in a directory short enough for the host's socket.
func newHome(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	command(t, dir, []string{"GOPROXY=off", "GOFLAGS=-mod=mod"}, bin, "init", "--source", repo(t))
	return dir
}

// until polls cond.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(waitLimit); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// host is a running oikade host. command is how it's started, and client how anything else talks
// to it.
type host struct {
	t      *testing.T
	start  func() *exec.Cmd
	client func(args ...string) *exec.Cmd
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
}

// launch starts the host.
func (h *host) launch() {
	t := h.t
	t.Helper()
	h.cmd = h.start()
	var err error
	if h.stdin, err = h.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := h.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h.cmd.Stderr = h.cmd.Stdout
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.done = make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			t.Log(lines.Text())
		}
	}(h.done)
}

// stop ends the host, as a service manager does.
func (h *host) stop() {
	h.t.Helper()
	_ = h.stdin.Close()
	_ = h.cmd.Process.Signal(os.Interrupt)
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
}

// change changes a simulated device.
func (h *host) change(device, capability string, value any) {
	h.t.Helper()
	raw, err := json.Marshal(map[string]any{"device": device, "capability": capability, "value": value})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.stdin.Write(append(raw, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

// status is oikade status, and false if the host isn't answering yet.
func (h *host) status() (api.Status, bool) {
	var s api.Status
	out, err := h.client("status", "--json").Output()
	return s, err == nil && json.Unmarshal(out, &s) == nil
}

// value is a device's value, or nil.
func (h *host) value(device, capability string) any {
	s, ok := h.status()
	if !ok {
		return nil
	}
	i := slices.IndexFunc(s.Devices, func(d api.Device) bool { return d.Name == device })
	if i < 0 {
		return nil
	}
	caps := s.Devices[i].Capabilities
	if j := slices.IndexFunc(caps, func(c api.Capability) bool { return c.Name == capability }); j >= 0 {
		return caps[j].Value
	}
	return nil
}

// freePort is a TCP port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// metrics returns the host's Prometheus page, or "" while it isn't up.
func metrics(ctx context.Context, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = res.Body.Close() }()
	var b bytes.Buffer
	_, _ = io.Copy(&b, res.Body)
	return b.String()
}

// From oikade init to a rule running on a host, then a restart that changes nothing.
func TestANewHomeRunsOnAHost(t *testing.T) {
	bin := oikade(t)
	module := newHome(t, bin)
	command(t, module, []string{"GOPROXY=off", "GOFLAGS=-mod=mod"}, bin, "test")
	command(t, module, []string{"GOPROXY=off", "GOFLAGS=-mod=mod"}, bin, "check")

	devices, err := filepath.Abs(filepath.Join("testdata", "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir: a Unix socket's path has to fit in about 100 bytes.
	data, err := os.MkdirTemp("", "oik")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(data) })
	address := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	h := &host{t: t,
		start: func() *exec.Cmd {
			return exec.Command(bin, "host", "--data-dir", data, "--simulate", devices,
				"--matter-port=0", "--metrics-addr="+address)
		},
		client: func(args ...string) *exec.Cmd { return exec.Command(bin, append(args, "--data-dir", data)...) },
	}
	h.launch()
	t.Cleanup(h.stop)
	until(t, "the host", func() bool { _, ok := h.status(); return ok })

	deploy := h.client("deploy", "--module", module)
	deploy.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=mod")
	if out, err := deploy.CombinedOutput(); err != nil {
		t.Fatalf("oikade deploy: %v\n%s", err, out)
	}
	until(t, "your process", func() bool { s, ok := h.status(); return ok && s.Process })

	h.change("porch-sensor", "sensor.occupancy-detected", true)
	until(t, "the porch light", func() bool { return h.value("porch-light", "light.on") == true })
	page := "http://" + address + "/metrics"
	until(t, "the metrics Grafana reads", func() bool {
		return strings.Contains(metrics(t.Context(), page),
			`oikade_capability_value{capability="light.on",device="porch-light"`)
	})

	h.stop()
	h.launch()
	until(t, "your process after a restart", func() bool { s, ok := h.status(); return ok && s.Process })
	if s, _ := h.status(); s.Version == "" {
		t.Errorf("the deployed version didn't outlive the restart: %+v", s)
	}
}

const (
	// chipNode is the node ID chip-tool gives the bridge.
	chipNode = "0x1122"
	// inContainer is where the journey's data directory is in the container.
	inContainer = "/var/lib/oikade"
)

// chipValue matches the value chip-tool logs for an attribute it reads.
var chipValue = regexp.MustCompile(`\[TOO\]\s+[A-Za-z]+: ([A-Za-z0-9.-]+)`)

// docker runs docker, and fails the test if it fails.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	return command(t, "", nil, "docker", args...)
}

// chipTool starts chip-tool's container, and skips without an image. Its network has IPv6, as a home's
// does: chip-tool resolves a node to IPv6 addresses only, and Docker gives a container none by default.
func chipTool(t *testing.T) string {
	t.Helper()
	image, ok := os.LookupEnv("OIKADE_CHIP_TOOL_IMAGE")
	if !ok {
		t.Skip("OIKADE_CHIP_TOOL_IMAGE isn't set: make test-journeys builds it")
	}
	name := fmt.Sprintf("oikade-journey-%d", time.Now().UnixNano())
	docker(t, "network", "create", "--ipv6", "--subnet", "fd6f:696b:6164::/64", name)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
	docker(t, "run", "-d", "--name", name, "--network", name, image)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	return name
}

// chip runs chip-tool and returns what it printed.
func chip(container string, args ...string) string {
	out, _ := exec.Command("docker", append([]string{"exec", container, "chip-tool"}, args...)...).CombinedOutput()
	return string(out)
}

// chipRead reads an attribute with chip-tool and returns its last logged value, or "".
func chipRead(container, cluster, attribute, endpoint string) string {
	out := chip(container, cluster, "read", attribute, chipNode, endpoint)
	m := chipValue.FindAllStringSubmatch(out, -1)
	if m == nil {
		return ""
	}
	return m[len(m)-1][1]
}

// A controller nobody here wrote pairs with a host, drives a device, reads a change from one, and is
// still paired after the host restarts. Both run in chip-tool's container, whose Avahi finds the host.
func TestChipToolPairsAndDrivesADevice(t *testing.T) {
	container := chipTool(t)
	arch := strings.TrimSpace(docker(t, "version", "-f", "{{.Server.Arch}}"))
	target := []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch}
	native := oikade(t)
	module := newHome(t, native)
	bin := oikade(t, target...)
	binary := filepath.Join(t.TempDir(), "home")
	build := slices.Concat(target, []string{"GOPROXY=off", "GOFLAGS=-mod=mod"})
	command(t, module, build, "go", "build", "-o", binary, ".")
	docker(t, "cp", bin, container+":/usr/local/bin/oikade")
	docker(t, "cp", binary, container+":/usr/local/bin/home")
	docker(t, "cp", filepath.Join("testdata", "devices.json"), container+":/devices.json")

	h := &host{t: t,
		start: func() *exec.Cmd {
			return exec.Command("docker", "exec", "-i", container, "oikade", "host", "--data-dir", inContainer,
				"--home", "/usr/local/bin/home", "--simulate", "/devices.json", "--metrics-addr=127.0.0.1:0")
		},
		client: func(args ...string) *exec.Cmd {
			args = append(args, "--data-dir", inContainer)
			return exec.Command("docker", append([]string{"exec", container, "oikade"}, args...)...)
		},
	}
	h.launch()
	// docker exec passes on no signal, and exiting says nothing of the process inside, whose lock on
	// the store the next host needs.
	stop := func() {
		_ = exec.Command("docker", "exec", container, "pkill", "-INT", "-x", "oikade").Run()
		h.stop()
		until(t, "the host to stop", func() bool {
			return exec.Command("docker", "exec", container, "pgrep", "-x", "oikade").Run() != nil
		})
	}
	t.Cleanup(stop)

	var bridge api.Matter
	until(t, "the commissioning window", func() bool {
		out, err := h.client("matter", "--json").Output()
		return err == nil && json.Unmarshal(out, &bridge) == nil && bridge.Window
	})
	until(t, "chip-tool to pair", func() bool {
		out := chip(container, "pairing", "code", chipNode, bridge.ManualCode)
		return strings.Contains(out, "Device commissioning completed with success")
	})

	endpoint := func(name string) string {
		out, err := h.client("devices", "--json").Output()
		if err != nil {
			t.Fatal(err)
		}
		var list registry.Snapshot
		if err := json.Unmarshal(out, &list); err != nil {
			t.Fatalf("%v in %s", err, out)
		}
		i := slices.IndexFunc(list.Devices, func(d registry.Device) bool { return d.Name == name })
		if i < 0 {
			t.Fatalf("no %s in %s", name, out)
		}
		return fmt.Sprint(list.Devices[i].Endpoint)
	}
	light, shed := endpoint("porch-light"), endpoint("shed-temperature")

	until(t, "chip-tool to read the light", func() bool {
		return chipRead(container, "onoff", "on-off", light) == "FALSE"
	})
	docker(t, "exec", container, "chip-tool", "onoff", "toggle", chipNode, light)
	until(t, "the host to have the write", func() bool { return h.value("porch-light", "light.on") == true })

	h.change("shed-temperature", "sensor.temperature", 21.5)
	until(t, "chip-tool to read the sensor", func() bool {
		return chipRead(container, "temperaturemeasurement", "measured-value", shed) == "2150"
	})

	stop()
	h.launch()
	until(t, "chip-tool to read after a restart, without pairing again", func() bool {
		return chipRead(container, "onoff", "on-off", light) != ""
	})
}
