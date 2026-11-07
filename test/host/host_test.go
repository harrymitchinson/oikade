// Package host tests the host and your process as what they are: separate programs. It builds
// oikade-host and oikadectl, and a home as a module of its own, then starts, kills and freezes them.
//
// Nothing here orders events with a sleep. A test waits for a line of the host's log, and sync
// proves the process has caught up.
package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/wire"
)

const (
	wrote     = "Wrote simulated device"
	started   = "Started automations process"
	attached  = "Connected automations process"
	waitLimit = 45 * time.Second
)

var (
	buildOnce sync.Once
	buildErr  error
	buildDir  string
	hostBin   string
	ctlBin    string
	homeBin   string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// build builds the binaries once. The home goes in a module of its own, outside this one, so it
// compiles only if the public framework is enough.
func build(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "oikbin")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		hostBin, ctlBin = filepath.Join(dir, "oikade-host"), filepath.Join(dir, "oikadectl")
		homeBin = filepath.Join(dir, "home")
		repo, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			buildErr = err
			return
		}
		// Fault points are in these binaries alone: they compile to nothing in a normal build.
		args := []string{"build", "-tags", "faultpoints"}
		if raceBuild {
			args = append(args, "-race")
		}
		if buildErr = run(repo, nil, append(args, "-o", hostBin, "./cmd/oikade-host")...); buildErr != nil {
			return
		}
		if buildErr = run(repo, nil, append(args, "-o", ctlBin, "./cmd/oikadectl")...); buildErr != nil {
			return
		}
		module := filepath.Join(dir, "module")
		if buildErr = writeModule(repo, module); buildErr != nil {
			return
		}
		buildErr = run(module, []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}, append(args, "-o", homeBin, ".")...)
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

func run(dir string, env []string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// writeModule makes a module from the fixture: this module's dependencies, and this repo as one of
// them. Its path is unrelated to this repo's, so it can't import anything internal.
func writeModule(repo, module string) error {
	if err := os.MkdirAll(module, 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
		return err
	}
	mod := regexp.MustCompile(`(?m)^module .*$`).ReplaceAllString(string(raw), "module example.com/home")
	mod += "\nrequire github.com/harrymitchinson/oikade v0.0.0\n"
	mod += "\nreplace github.com/harrymitchinson/oikade => " + repo + "\n"
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte(mod), 0o600); err != nil {
		return err
	}
	for from, to := range map[string]string{"go.sum": "go.sum", "test/fixtures/home/main.go": "main.go"} {
		raw, err := os.ReadFile(filepath.Join(repo, from))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(module, to), raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// line is a line of the host's log, or a process's.
type line map[string]any

func (l line) msg() string { s, _ := l["msg"].(string); return s }

func (l line) is(msg string, fields ...any) bool {
	if l.msg() != msg {
		return false
	}
	for i := 0; i+1 < len(fields); i += 2 {
		if l[fields[i].(string)] != fields[i+1] {
			return false
		}
	}
	return true
}

// rig is a running host.
type rig struct {
	t   *testing.T
	dir string
	// env is added to the host's environment, which its process inherits.
	env   []string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	exit  chan error

	mu    sync.Mutex
	lines []line
	grew  chan struct{}
	seen  int
	probe bool
}

func start(t *testing.T, args ...string) *rig {
	t.Helper()
	r := prepare(t)
	r.launch(args...)
	r.waitFor(func(l line) bool { return l.is(attached) })
	return r
}

// prepare makes a rig with a data directory and no host yet, so a test can set it up first.
func prepare(t *testing.T) *rig {
	t.Helper()
	build(t)
	// Not t.TempDir: a Unix socket's path has to fit in about 100 bytes.
	dir, err := os.MkdirTemp("", "oik")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return &rig{t: t, dir: dir, grew: make(chan struct{})}
}

// launch starts a host on the rig's data directory.
func (r *rig) launch(args ...string) {
	t := r.t
	t.Helper()
	r.launchWithout(append([]string{"--home", homeBin, "--simulate", devicesFile(t)}, args...)...)
}

func devicesFile(t *testing.T) string {
	t.Helper()
	devices, err := filepath.Abs(filepath.Join("testdata", "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	return devices
}

// launchWithout starts a host with only these flags.
func (r *rig) launchWithout(args ...string) {
	t := r.t
	t.Helper()
	r.exit = make(chan error, 1)
	r.probe = false
	base := make([]string, 0, 4+len(args))
	base = append(base, "--data-dir", r.dir, "--metrics-addr=127.0.0.1:0", "--matter-port=0")
	cmd := exec.Command(hostBin, append(base, args...)...)
	cmd.Env = append(os.Environ(), r.env...)
	r.cmd = cmd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if r.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exit := r.exit
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exit:
			exit <- err
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	reading := make(chan struct{})
	go func() {
		defer close(reading)
		r.read(stdout)
	}()
	go func() {
		<-reading
		exit <- cmd.Wait()
	}()
}

func (r *rig) read(out io.Reader) {
	lines := bufio.NewScanner(out)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		var l line
		if json.Unmarshal(lines.Bytes(), &l) != nil {
			l = line{"msg": "unparsed", "text": lines.Text()}
		}
		r.mu.Lock()
		r.lines = append(r.lines, l)
		close(r.grew)
		r.grew = make(chan struct{})
		r.mu.Unlock()
	}
}

// waitFor returns once a line matches, after the last one a wait matched.
func (r *rig) waitFor(match func(line) bool) {
	r.t.Helper()
	timeout := time.After(waitLimit)
	for {
		r.mu.Lock()
		for ; r.seen < len(r.lines); r.seen++ {
			if l := r.lines[r.seen]; match(l) {
				r.seen++
				r.mu.Unlock()
				return
			}
		}
		grew := r.grew
		r.mu.Unlock()
		select {
		case <-grew:
		case <-timeout:
			r.t.Fatalf("no matching log line in %s. The log:\n%s", waitLimit, r.dump())
		}
	}
}

func (r *rig) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, l := range r.lines {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// count returns how many lines so far match.
func (r *rig) count(msg string, fields ...any) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if l.is(msg, fields...) {
			n++
		}
	}
	return n
}

// change changes a simulated device, and waits for the host to have it.
func (r *rig) change(device, capability string, value any) {
	r.t.Helper()
	raw, err := json.Marshal(map[string]any{"device": device, "capability": capability, "value": value})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor(func(l line) bool { return l.is("Changed capability", "device", device, "capability", capability) })
}

// sync returns once your process has answered a change, twice over. A write it started before the
// first answer has reached the host by the second, so a test can then count writes.
func (r *rig) sync() {
	r.t.Helper()
	for range 2 {
		r.probe = !r.probe
		before := r.count(wrote, "device", "probe-out")
		r.send("probe", "sensor.contact-open", r.probe)
		timeout := time.After(waitLimit)
		for r.count(wrote, "device", "probe-out") == before {
			r.mu.Lock()
			grew := r.grew
			r.mu.Unlock()
			if r.count(wrote, "device", "probe-out") != before {
				break
			}
			select {
			case <-grew:
			case <-timeout:
				r.t.Fatalf("the process never answered. The log:\n%s", r.dump())
			}
		}
	}
}

func (r *rig) send(device, capability string, value any) {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"device": device, "capability": capability, "value": value})
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatal(err)
	}
}

// awaitCount waits for this many matching lines, wherever they are in the log.
func (r *rig) awaitCount(want int, msg string, fields ...any) {
	r.t.Helper()
	timeout := time.After(waitLimit)
	for {
		r.mu.Lock()
		grew := r.grew
		r.mu.Unlock()
		if r.count(msg, fields...) >= want {
			return
		}
		select {
		case <-grew:
		case <-timeout:
			r.t.Fatalf("waited for %d of %q. The log:\n%s", want, msg, r.dump())
		}
	}
}

func (r *rig) wrote(device string, value any) {
	r.t.Helper()
	r.waitFor(func(l line) bool { return l.is(wrote, "device", device, "value", value) })
}

// pid returns the process the last started line names.
func (r *rig) pid() int {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range slices.Backward(r.lines) {
		if l := v; l.is(started) {
			if pid, ok := l["pid"].(float64); ok {
				return int(pid)
			}
		}
	}
	r.t.Fatal("no process has started")
	return 0
}

// kill kills your process, and waits for the next one to start and connect.
func (r *rig) kill() {
	r.t.Helper()
	old := r.pid()
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		r.t.Fatal(err)
	}
	r.restarted(old)
}

func (r *rig) restarted(old int) {
	r.t.Helper()
	r.waitFor(func(l line) bool {
		pid, _ := l["pid"].(float64)
		return l.is(started) && pid != 0 && int(pid) != old
	})
	r.waitFor(func(l line) bool { return l.is(attached) })
}

func (r *rig) hostRunning() {
	r.t.Helper()
	select {
	case err := <-r.exit:
		r.exit <- err
		r.t.Fatalf("the host stopped: %v\n%s", err, r.dump())
	default:
	}
}

func TestHomeReactsAndWritesThroughTheHost(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("bathroom-sensor", "sensor.relative-humidity", 80)
	r.wrote("fan", true)
	r.change("bathroom-sensor", "sensor.relative-humidity", 50)
	r.wrote("fan", false)
}

func TestKilledProcessLeavesTheHostAndRepeatsNothing(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.change("bathroom-sensor", "sensor.relative-humidity", 80)
	r.wrote("fan", true)
	r.sync()

	// The hold is active, so a new process has nothing to do.
	r.kill()
	r.sync()
	r.hostRunning()
	if n := r.count(wrote, "device", "fan", "value", true); n != 1 {
		t.Fatalf("fan was started %d times, want 1\n%s", n, r.dump())
	}

	// The hold's condition ends with no process running, so the next one ends it, once. The host
	// keeps taking changes meanwhile.
	old := r.pid()
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	r.waitFor(func(l line) bool { return l.is("Restarting automations process") })
	r.change("bathroom-sensor", "sensor.relative-humidity", 40)
	r.hostRunning()
	r.restarted(old)
	r.wrote("fan", false)
	r.sync()
	if n := r.count(wrote, "device", "fan", "value", false); n != 1 {
		t.Fatalf("fan was stopped %d times, want 1\n%s", n, r.dump())
	}
}

// The kill lands as soon as the host reports the write, so it falls anywhere around the point
// where the hold's state is saved. Whichever side it lands on, the latch must hold.
func TestLatchedSafetyHoldSurvivesAKillMidWay(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	r.send("leak-sensor", "sensor.contact-open", true)
	r.wrote("valve", false)
	r.kill()
	r.sync()

	// The leak has stopped, and the hold is latched, so nothing changes.
	r.change("leak-sensor", "sensor.contact-open", false)
	r.sync()
	r.kill()
	r.sync()
	if n := r.count(wrote, "device", "valve", "value", true); n != 0 {
		t.Fatalf("valve reopened while latched\n%s", r.dump())
	}
	if n := r.count(wrote, "device", "valve", "value", false); n != 1 {
		t.Fatalf("valve was closed %d times, want 1\n%s", n, r.dump())
	}

	// Only a release ends it. The siren has no arbiter, so its end proves the latch itself came back
	// from the host: a hold that forgot it would have nothing to end.
	if n := r.count(wrote, "device", "siren", "value", false); n != 0 {
		t.Fatalf("siren stopped while latched\n%s", r.dump())
	}
	r.change("reset", "sensor.contact-open", true)
	r.wrote("valve", true)
	// Your process ends the hold in its own time after the release, which sync doesn't wait for, and
	// it can land either side of the host's write to the valve.
	r.awaitCount(1, wrote, "device", "siren", "value", false)
	r.sync()
	if n := r.count(wrote, "device", "siren", "value", false); n != 1 {
		t.Fatalf("siren was stopped %d times, want 1\n%s", n, r.dump())
	}
}

func TestFrozenProcessIsKilledAfterTenSeconds(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	old := r.pid()
	froze := time.Now()
	if err := syscall.Kill(old, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	r.waitFor(func(l line) bool { return l.is("Killing automations process; it stopped answering") })
	if took := time.Since(froze); took < 8*time.Second || took > 15*time.Second {
		t.Fatalf("killed after %s, want about 10s", took)
	}
	r.restarted(old)
	r.sync()
}

func TestStuckConditionCannotStopTheHost(t *testing.T) {
	t.Parallel()
	r := start(t, "--heartbeat-timeout=3s")
	r.sync()
	old := r.pid()
	r.change("trap", "sensor.contact-open", true)

	// With the condition spinning, the host still takes changes and refuses a second process.
	r.change("bathroom-sensor", "sensor.relative-humidity", 45)
	raw, err := net.Dial("unix", filepath.Join(r.dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	conn := wire.NewConn(raw)
	defer func() { _ = conn.Close() }()
	hello := wire.Message{Type: wire.TypeHello, Version: wire.Version, Role: wire.RoleAutomations}
	if err := conn.Write(hello); err != nil {
		t.Fatal(err)
	}
	if m, err := conn.Read(); err != nil || m.Type != wire.TypeRejected {
		t.Fatalf("got %+v, %v", m, err)
	}
	r.hostRunning()

	r.waitFor(func(l line) bool { return l.is("Killing automations process; it stopped answering") })
	r.change("trap", "sensor.contact-open", false)
	r.restarted(old)
	r.sync()
}

func TestSecondProcessCannotConnect(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	out, err := second(r)
	if err == nil || !strings.Contains(out, "already connected") {
		t.Fatalf("got %v: %s", err, out)
	}
	r.sync()
	if n := r.count(started); n != 1 {
		t.Fatalf("%d processes started, want 1", n)
	}
}

// second runs another copy of the home against the host, as if started by hand.
func second(r *rig) (string, error) {
	ctx, cancel := context.WithTimeout(r.t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, homeBin, "automations")
	cmd.Env = append(os.Environ(), "OIKADE_SOCKET="+filepath.Join(r.dir, "host.sock"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestProcessStopsWhenTheHostDies(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	pid := r.pid()
	if err := r.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitLimit)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHomeRefusesToRunWithoutTheHost(t *testing.T) {
	t.Parallel()
	build(t)
	cmd := exec.Command(homeBin)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "oikade-host") {
		t.Fatalf("got %v: %s", err, out)
	}
}

// binFor is the binary a command belongs to: oikade-host trusts a client on the host's machine, and
// oikadectl does everything else.
func binFor(args []string) string {
	if len(args) > 1 && args[0] == "client" && args[1] == "add" {
		return hostBin
	}
	return ctlBin
}
