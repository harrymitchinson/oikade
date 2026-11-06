package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const pauseChanged = "Changed rule pause"

func (r *rig) status(args ...string) api.Status {
	r.t.Helper()
	var st api.Status
	out := r.stdout(nil, append([]string{"status", "--json"}, args...)...)
	if err := json.Unmarshal(out, &st); err != nil {
		r.t.Fatalf("reading status: %v: %s", err, out)
	}
	return st
}

// stdout runs a command that must succeed.
func (r *rig) stdout(env []string, args ...string) []byte {
	r.t.Helper()
	args = append(args[:1:1], append([]string{"--data-dir", r.dir}, args[1:]...)...)
	cmd := exec.Command(hostBin, args...)
	cmd.Env = append(os.Environ(), env...)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("oikade %v: %v: %s", args, err, errOut.String())
	}
	return out
}

func onOf(t *testing.T, st api.Status, device string) api.Capability {
	t.Helper()
	for _, d := range st.Devices {
		if d.Name != device {
			continue
		}
		for _, c := range d.Capabilities {
			if c.Name == "switch.on" {
				return c
			}
		}
	}
	t.Fatalf("no switch.on on %s in %+v", device, st)
	return api.Capability{}
}

func winner(c api.Capability) string {
	for _, claim := range c.Claims {
		if claim.Winner {
			return claim.Source
		}
	}
	return ""
}

func TestStatusSaysWhyAndSetGoesThroughArbitration(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()

	st := r.status()
	if !st.Process {
		t.Errorf("status says no process: %+v", st)
	}
	if c := onOf(t, st, "fan"); c.Value != false || c.Level != "" {
		t.Errorf("fan: %+v", c)
	}

	r.stdout(nil, "set", "claimed", "switch.on", "off", "--json")
	r.wrote("claimed", false)
	c := onOf(t, r.status(), "claimed")
	if c.Value != false || c.Level != "Override" || winner(c) != "Command" {
		t.Errorf("after set: %+v", c)
	}

	r.change("leak-sensor", "sensor.contact-open", true)
	r.waitFor(func(l line) bool { return l.is(wrote, "device", "valve", "value", false) })
	r.sync()
	c = onOf(t, r.status(), "valve")
	if c.Level != "Safety" || winner(c) != "Rule/leak-shutoff" {
		t.Errorf("a Safety hold: %+v", c)
	}
	stderr, err := r.oikade(nil, "set", "valve", "switch.on", "on")
	if err == nil || !strings.Contains(stderr, "Rule/leak-shutoff holds valve switch.on at Safety") {
		t.Errorf("set against Safety: %v: %s", err, stderr)
	}
	if v := onOf(t, r.status(), "valve").Value; v != false {
		t.Errorf("the valve moved: %v", v)
	}
	if _, err := r.oikade(nil, "set", "nothing", "switch.on", "on"); err == nil {
		t.Error("set an unknown device")
	}
	if _, err := r.oikade(nil, "set", "fan", "switch.on", "12"); err == nil {
		t.Error("set a bad value")
	}
}

func TestStatusNamesOneDevice(t *testing.T) {
	t.Parallel()
	r := start(t)
	if st := r.status("fan"); len(st.Devices) != 1 || st.Devices[0].Name != "fan" {
		t.Errorf("got %+v", st.Devices)
	}
	if !strings.Contains(string(r.stdout(nil, "status", "valve")), "valve") {
		t.Error("the table left out the valve")
	}
	if _, err := r.oikade(nil, "status", "nothing"); err == nil {
		t.Error("a device that doesn't exist was fine")
	}
}

func paused(t *testing.T, st api.Status, rule string) bool {
	t.Helper()
	for _, r := range st.Rules {
		if r.Name == rule {
			return r.Paused
		}
	}
	t.Fatalf("no rule %s in %+v", rule, st.Rules)
	return false
}

func TestPauseLastsAcrossADeployAndARestartUntilResumed(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	r.stdout(nil, "pause", "fan")
	r.waitFor(func(l line) bool { return l.is(pauseChanged, "rule", "fan") })
	r.sync()

	r.change("bathroom-sensor", "sensor.relative-humidity", 80)
	r.sync()
	if n := r.count(wrote, "device", "fan"); n != 0 {
		t.Fatalf("a paused hold wrote %d times", n)
	}

	r.mustDeploy("second")
	r.sync()
	if n := r.count(wrote, "device", "fan"); n != 0 {
		t.Fatalf("a paused hold wrote after a deploy")
	}
	if !paused(t, r.status(), "fan") {
		t.Error("a deploy ended the pause")
	}

	r.stop()
	r.launch(settle)
	r.waitFor(func(l line) bool { return l.is(attached) })
	r.sync()
	if !paused(t, r.status(), "fan") {
		t.Error("a restart ended the pause")
	}
	r.change("bathroom-sensor", "sensor.relative-humidity", 90)
	r.sync()
	if n := r.count(wrote, "device", "fan"); n != 0 {
		t.Fatalf("a paused hold wrote after a restart")
	}

	r.stdout(nil, "resume", "fan")
	r.wrote("fan", true)
	if paused(t, r.status(), "fan") {
		t.Error("still paused")
	}
	if _, err := r.oikade(nil, "resume", "fan"); err == nil {
		t.Error("resumed a rule that isn't paused")
	}
}

func TestPausingAMomentStopsItAndEndsItsClaims(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("claim-go", "sensor.contact-open", true)
	r.wrote("claimed", false)
	r.sync()
	if c := onOf(t, r.status(), "claimed"); winner(c) != "Rule/claim-moment" {
		t.Fatalf("no claim to end: %+v", c)
	}
	r.stdout(nil, "pause", "claim-moment")
	r.wrote("claimed", true)
	r.sync()
	writes := r.count(wrote, "device", "claimed")
	r.change("claim-go", "sensor.contact-open", false)
	r.change("claim-go", "sensor.contact-open", true)
	r.sync()
	if n := r.count(wrote, "device", "claimed"); n != writes {
		t.Fatal("a paused moment ran")
	}
	r.stdout(nil, "resume", "claim-moment")
	r.waitFor(func(l line) bool { return l.is(pauseChanged, "rule", "claim-moment", "paused", false) })
	r.sync()
	r.change("claim-go", "sensor.contact-open", false)
	r.change("claim-go", "sensor.contact-open", true)
	r.wrote("claimed", false)
}

func TestPausingASafetyRuleNeedsAskingAndKeepsItsClaim(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("leak-sensor", "sensor.contact-open", true)
	r.wrote("valve", false)
	r.sync()

	stderr, err := r.oikade(nil, "pause", "leak-shutoff")
	if err == nil || !strings.Contains(stderr, "--allow-safety") {
		t.Errorf("paused a Safety rule without asking: %v: %s", err, stderr)
	}
	r.stdout(nil, "pause", "leak-shutoff", "--allow-safety")
	r.sync()
	if c := onOf(t, r.status(), "valve"); winner(c) != "Rule/leak-shutoff" || c.Value != false {
		t.Errorf("the pause ended a Safety claim: %+v", c)
	}
	if _, err := r.oikade(nil, "pause", "no-such-rule"); err == nil {
		t.Error("paused a rule that doesn't exist")
	}
}

func TestPausingAFlowStopsItsRunAndEndsItsClaims(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("claim-flow-go", "sensor.contact-open", true)
	r.wrote("claimed-flow", false)
	r.stdout(nil, "pause", "claiming")
	r.wrote("claimed-flow", true)
	r.sync()

	// A resumed flow starts from a new trigger.
	r.stdout(nil, "resume", "claiming")
	r.waitFor(func(l line) bool { return l.is(pauseChanged, "rule", "claiming", "paused", false) })
	r.sync()
	r.change("claim-flow-go", "sensor.contact-open", false)
	r.change("claim-flow-go", "sensor.contact-open", true)
	r.wrote("claimed-flow", false)
}

// tailing reads oikade tail --json until the test ends.
func (r *rig) tailing(env []string, args ...string) <-chan api.Event {
	r.t.Helper()
	cmd := exec.Command(hostBin, append([]string{"tail", "--json", "--data-dir", r.dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	events := make(chan api.Event, 1024)
	go func() {
		defer close(events)
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			var e api.Event
			if err := json.Unmarshal(lines.Bytes(), &e); err == nil {
				events <- e
			}
		}
	}()
	return events
}

func awaitEvent(t *testing.T, events <-chan api.Event, match func(api.Event) bool) {
	t.Helper()
	deadline := time.After(waitLimit)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("the tail ended")
			}
			if match(e) {
				return
			}
		case <-deadline:
			t.Fatal("no such event came")
		}
	}
}

// tailStarted changes a device until the tail hears it, since oikade tail has no way to say it's on.
func tailStarted(r *rig, events <-chan api.Event) {
	r.t.Helper()
	deadline := time.After(waitLimit)
	for v := 60; ; v++ {
		r.send("bathroom-sensor", "sensor.relative-humidity", v%10+50)
		select {
		case e, ok := <-events:
			if !ok {
				r.t.Fatal("the tail ended")
			}
			if e.Type == api.EventChange {
				return
			}
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			r.t.Fatal("the tail never started")
		}
	}
}

func TestTailStreamsChangesAndRunsAcrossAProcessRestart(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	events := r.tailing(nil)
	tailStarted(r, events)

	r.change("claim-go", "sensor.contact-open", true)
	awaitEvent(t, events, func(e api.Event) bool { return e.Type == api.EventRule && e.Rule == "claim-moment" })

	r.kill()
	awaitEvent(t, events, func(e api.Event) bool { return e.Type == api.EventProcess && e.Message == "disconnected" })
	awaitEvent(t, events, func(e api.Event) bool { return e.Type == api.EventProcess && e.Message == "connected" })

	r.change("leak-sensor", "sensor.contact-open", true)
	awaitEvent(t, events, func(e api.Event) bool {
		return e.Type == api.EventChange && e.Device == "leak-sensor" && e.Value == true
	})
}

// fakeSSH is an ssh that runs its command here, as sshd would, after checking how it was called.
func fakeSSH(t *testing.T) []string {
	t.Helper()
	dir, err := os.MkdirTemp("", "oikssh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	script := `#!/bin/sh
[ "$1" = -T ] && [ "$2" = -- ] && [ "$3" = pi ] || { echo "bad ssh arguments: $*" >&2; exit 255; }
exec sh -c "$4"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := []string{dir, buildDir, os.Getenv("PATH")}
	return []string{"PATH=" + strings.Join(path, string(os.PathListSeparator))}
}

func TestHostFlagRunsEveryCommandOverSSH(t *testing.T) {
	t.Parallel()
	r := start(t, settle)
	r.sync()
	env := fakeSSH(t)
	remote := []string{"--host", "pi"}

	st := r.stdout(env, append([]string{"status", "--json"}, remote...)...)
	if !strings.Contains(string(st), `"fan"`) {
		t.Errorf("status over SSH: %s", st)
	}
	r.stdout(env, append([]string{"set", "claimed", "switch.on", "off"}, remote...)...)
	r.wrote("claimed", false)
	r.stdout(env, append([]string{"pause", "fan"}, remote...)...)
	r.stdout(env, append([]string{"resume", "fan"}, remote...)...)

	events := r.tailing(env, remote...)
	tailStarted(r, events)

	r.stdout(env, append([]string{"deploy", "--binary", variant(t, "second")}, remote...)...)
	r.waitFor(func(l line) bool { return l.is(deployed) })

	// The flag can come first too, and a failure on the far side comes back.
	cmd := exec.Command(hostBin, "--host", "pi", "status", "--data-dir", filepath.Join(r.dir, "nothing"))
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err == nil {
		t.Error("reached a host that isn't there")
	}
	cmd = exec.Command(hostBin, "--host", "pi", "status", "--json", "--data-dir", r.dir)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), `"fan"`) {
		t.Errorf("--host before the command: %v: %s", err, out)
	}
}

func TestMetricsAndHealthAreServedOverHTTP(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	var address string
	r.mu.Lock()
	for _, l := range r.lines {
		if l.is("Serving metrics and health") {
			address, _ = l["address"].(string)
		}
	}
	r.mu.Unlock()
	fetch := func(path string) string {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://%s%s", address, path))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if got := fetch("/healthz"); got != "ok\n" {
		t.Errorf("healthz: %q", got)
	}
	if got := fetch("/metrics"); !strings.Contains(got, `oikade_capability_value{`) ||
		!strings.Contains(got, `oikade_device_control_level{`) {
		t.Errorf("host metrics missing: %s", got)
	}
	// Rules run in your process, which reports every few seconds.
	r.change("bathroom-sensor", "sensor.relative-humidity", 80)
	deadline := time.Now().Add(waitLimit)
	for !strings.Contains(fetch("/metrics"), "oikade_rule_active{") {
		if time.Now().After(deadline) {
			t.Fatal("your process's rule metrics never reached the host")
		}
		time.Sleep(200 * time.Millisecond)
	}
	resp, err := http.Post(fmt.Sprintf("http://%s/metrics", address), "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("the listener took a POST: %d", resp.StatusCode)
	}
}
