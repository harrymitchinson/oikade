// Package install runs oikade install on a machine: a container with systemd, reached through a
// stand-in for ssh, so that it goes the way oikade install --host does on a real one.
package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	image     = "oikade-install-test"
	machine   = "pi"
	unitFile  = "/etc/systemd/system/oikade.service"
	dataDir   = "/var/lib/oikade"
	waitLimit = 60 * time.Second
)

// rig is a running machine, and an oikade on this one that reaches it with --host.
type rig struct {
	t         *testing.T
	container string
	oikade    string
	next      string
	env       []string
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func start(t *testing.T) *rig {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("OIKADE_REQUIRE_DOCKER") != "" {
			t.Fatalf("no Docker: %v", err)
		}
		t.Skip("no Docker")
	}
	docker(t, "build", "-q", "-t", image, "testdata")
	name := fmt.Sprintf("oikade-install-%d", time.Now().UnixNano())
	docker(t, "run", "-d", "--name", name, "--privileged", "--cgroupns=host",
		"--tmpfs", "/run", "--tmpfs", "/run/lock", image)
	t.Cleanup(func() {
		if t.Failed() {
			journal := exec.Command("docker", "exec", name, "journalctl", "-u", "oikade", "--no-pager", "-n", "50")
			out, _ := journal.CombinedOutput()
			t.Logf("the host's log:\n%s", out)
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	dir := t.TempDir()
	r := &rig{t: t, container: name}

	arch := docker(t, "version", "--format", "{{.Server.Arch}}")
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	build := func(out, version, goos, goarch string) {
		cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", out, "./cmd/oikade")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building oikade: %v\n%s", err, out)
		}
	}
	// Two builds that differ, for the machine. Their platform isn't this one's on a Mac, so --binary.
	build(filepath.Join(dir, "linux-a"), "v0.0.1", "linux", arch)
	build(filepath.Join(dir, "linux-b"), "v0.0.2", "linux", arch)
	build(filepath.Join(dir, "oikade"), "dev", runtime.GOOS, runtime.GOARCH)
	r.oikade, r.next = filepath.Join(dir, "oikade"), filepath.Join(dir, "linux-b")

	// ssh [-T|-t] -- HOST COMMAND, which runs the command on the machine as root.
	stub := fmt.Sprintf(`#!/bin/sh
shift 3
exec docker exec -i %s /bin/sh -c "$1"
`, name)
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	r.env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r.until("systemd", func() bool {
		out, _ := exec.Command("docker", "exec", name, "systemctl", "is-system-running").Output()
		state := strings.TrimSpace(string(out))
		return state == "running" || state == "degraded"
	})
	return r
}

func (r *rig) until(what string, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !ok() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// on runs a command on the machine.
func (r *rig) on(command string) string {
	r.t.Helper()
	return docker(r.t, "exec", r.container, "/bin/sh", "-c", command)
}

// run runs oikade here, against the machine.
func (r *rig) run(args ...string) (string, error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.oikade, args...)
	cmd.Env = r.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (r *rig) must(args ...string) string {
	r.t.Helper()
	out, err := r.run(args...)
	if err != nil {
		r.t.Fatalf("oikade %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// secret sets a secret, with the value on stdin as a script would.
func (r *rig) secret(name, value string) {
	r.t.Helper()
	cmd := exec.Command(r.oikade, "secret", "set", name, "--host", machine)
	cmd.Env, cmd.Stdin = r.env, strings.NewReader(value+"\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("oikade secret set: %v\n%s", err, out)
	}
}

func (r *rig) hostPID() string {
	r.t.Helper()
	return r.on("systemctl show -p MainPID --value oikade")
}

func TestInstallLeavesARunningHostAndASecondRunChangesNothing(t *testing.T) {
	r := start(t)
	first := r.must("install", "--host", machine, "--binary", filepath.Join(filepath.Dir(r.oikade), "linux-a"),
		"--", "--matter-port", "0", "--matter-name", "Our \"home\"")
	wanted := []string{"created user oikade", "installed /usr/local/bin/oikade", "wrote " + unitFile, "started"}
	for _, want := range wanted {
		if !strings.Contains(first, want) {
			t.Errorf("the first run didn't say %q:\n%s", want, first)
		}
	}
	if got := r.on("systemctl is-enabled oikade"); got != "enabled" {
		t.Errorf("is-enabled = %s", got)
	}
	if got := r.on("stat -c '%U %a' " + dataDir); got != "oikade 700" {
		t.Errorf("the data directory is %s", got)
	}
	pid, unit := r.hostPID(), r.on("stat -c %Y.%i "+unitFile)
	if pid == "0" {
		t.Fatal("no host process")
	}
	r.on("sleep 1")

	second := r.must("install", "--host", machine, "--binary", filepath.Join(filepath.Dir(r.oikade), "linux-a"),
		"--", "--matter-port", "0", "--matter-name", "Our \"home\"")
	if !strings.Contains(second, "Nothing to change") {
		t.Errorf("the second run changed something:\n%s", second)
	}
	if got := r.hostPID(); got != pid {
		t.Errorf("the second run restarted the host: %s, was %s", got, pid)
	}
	if got := r.on("stat -c %Y.%i " + unitFile); got != unit {
		t.Errorf("the second run wrote the unit: %s, was %s", got, unit)
	}

	// The CLI reaches what it installed, with the same --host.
	var st map[string]any
	if err := json.Unmarshal([]byte(r.must("status", "--host", machine, "--json")), &st); err != nil {
		t.Fatal(err)
	}

	// It comes back by itself after dying, which is the point of a unit.
	r.on("kill -9 " + pid)
	r.until("the host to come back", func() bool {
		out, err := r.run("status", "--host", machine, "--json")
		return err == nil && out != "" && r.hostPID() != pid && r.hostPID() != "0"
	})
}

func TestInstallRunsTheHostAsWhoeverRanSudo(t *testing.T) {
	r := start(t)
	r.on("useradd --create-home pi")
	dir := filepath.Dir(r.oikade)
	docker(t, "cp", filepath.Join(dir, "linux-a"), r.container+":/tmp/oikade-a")
	r.on("chmod 755 /tmp/oikade-a")
	// sudo sets SUDO_USER, which is what a Pi's default login gets.
	r.on(`SUDO_USER=pi /tmp/oikade-a install --binary /tmp/oikade-a -- --matter-port 0`)
	if got := r.on("systemctl show -p User --value oikade"); got != "pi" {
		t.Errorf("User = %s", got)
	}
	if got := r.on("su -s /bin/sh pi -c 'oikade status --json' | head -c 1"); got != "{" {
		t.Errorf("the host's user can't reach it: %q", got)
	}
}

func TestUpgradeBacksUpAndSaysHowLongTheHostWasDown(t *testing.T) {
	r := start(t)
	dir := filepath.Dir(r.oikade)
	r.must("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"), "--", "--matter-port", "0")
	before := r.hostPID()
	unit := r.on("cat " + unitFile)
	r.secret("api-key", "hunter2")

	out := r.must("upgrade", "--host", machine, "--binary", r.next)
	if !strings.Contains(out, "Backed up the store to "+dataDir+"/backups/") ||
		!strings.Contains(out, "The host was down for") {
		t.Errorf("it didn't say what it did:\n%s", out)
	}
	t.Log(out)
	if got := r.on("/usr/local/bin/oikade version"); got != "v0.0.2" {
		t.Errorf("the machine is on %s", got)
	}
	if got := r.hostPID(); got == before || got == "0" {
		t.Errorf("the host didn't restart: %s", got)
	}
	if got := r.on("cat " + unitFile); got != unit {
		t.Errorf("an upgrade changed the host's flags:\n%s", got)
	}
	if got := r.on("ls " + dataDir + "/backups | wc -l"); got != "1" {
		t.Errorf("%s backups", got)
	}
	if out := r.must("secret", "list", "--host", machine); !strings.Contains(out, "api-key") {
		t.Errorf("the secret went: %s", out)
	}
	if out := r.must("upgrade", "--host", machine, "--binary", r.next); !strings.Contains(out, "Already on this version") {
		t.Errorf("a second upgrade did something:\n%s", out)
	}
	if got := r.on("journalctl -u oikade --no-pager"); strings.Contains(got, "hunter2") {
		t.Error("the secret's value is in the host's log")
	}
}

func TestUpgradeGoesBackWhenTheNewVersionWontStart(t *testing.T) {
	r := start(t)
	dir := filepath.Dir(r.oikade)
	r.must("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"), "--", "--matter-port", "0")
	// It runs the upgrade, as the new binary does, and then won't run as the host.
	docker(t, "cp", r.next, r.container+":/opt/oikade-b")
	r.on("chmod 755 /opt/oikade-b")
	broken := filepath.Join(dir, "broken")
	script := "#!/bin/sh\n[ \"$1\" = host ] && exit 1\nexec /opt/oikade-b \"$@\"\n"
	if err := os.WriteFile(broken, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := r.run("upgrade", "--host", machine, "--binary", broken)
	if err == nil {
		t.Fatalf("a version that won't start upgraded:\n%s", out)
	}
	if !strings.Contains(out, "going back") {
		t.Errorf("it didn't say it went back:\n%s", out)
	}
	if got := r.on("/usr/local/bin/oikade version"); got != "v0.0.1" {
		t.Errorf("the machine is on %s", got)
	}
	if _, err := r.run("status", "--host", machine, "--json"); err != nil {
		t.Errorf("the old version isn't answering: %v", err)
	}
}

func TestUpgradeToABinaryThatWontRunChangesNothing(t *testing.T) {
	r := start(t)
	dir := filepath.Dir(r.oikade)
	r.must("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"), "--", "--matter-port", "0")
	pid := r.hostPID()
	broken := filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := r.run("upgrade", "--host", machine, "--binary", broken); err == nil {
		t.Fatalf("it upgraded:\n%s", out)
	}
	if got := r.hostPID(); got != pid {
		t.Errorf("the host restarted: %s, was %s", got, pid)
	}
	if got := r.on("/usr/local/bin/oikade version"); got != "v0.0.1" {
		t.Errorf("the machine is on %s", got)
	}
}

func TestUpgradeOfAStoppedHostLeavesBackupsWorking(t *testing.T) {
	r := start(t)
	dir := filepath.Dir(r.oikade)
	r.must("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"), "--", "--matter-port", "0")
	r.on("systemctl stop oikade")
	if out := r.must("upgrade", "--host", machine, "--binary", r.next); !strings.Contains(out, "Backed up the store") {
		t.Errorf("it didn't say it backed up:\n%s", out)
	}
	// The host writes here as its own user, and the upgrade made the directory as root.
	r.must("backup", "--host", machine)
	if got := r.on("ls " + dataDir + "/backups | wc -l"); got != "2" && got != "1" {
		t.Errorf("%s backups", got)
	}
}

func TestInstallCanBeRecoveredFromAHostThatNeverStarted(t *testing.T) {
	r := start(t)
	dir := filepath.Dir(r.oikade)
	if out, err := r.run("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"),
		"--data-dir", "/srv/oikade/data", "--", "--matter-port", "0"); err != nil {
		t.Fatalf("a data directory with parents to make: %v\n%s", err, out)
	}
	r.on("systemctl stop oikade && rm /srv/oikade/data/oikade.db")
	out := r.must("upgrade", "--host", machine, "--data-dir", "/srv/oikade/data", "--binary", r.next)
	if !strings.Contains(out, "nothing to back up") {
		t.Errorf("it didn't say so:\n%s", out)
	}
}

func TestInstallGivesARestoredDirectoryToTheHostsUser(t *testing.T) {
	r := start(t)
	dir := filepath.Dir(r.oikade)
	r.must("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"), "--", "--matter-port", "0")
	r.on("useradd --create-home pi && systemctl stop oikade")
	r.must("install", "--host", machine, "--binary", filepath.Join(dir, "linux-a"), "--user", "pi",
		"--", "--matter-port", "0")
	if got := r.on("stat -c %U " + dataDir + "/oikade.db"); got != "pi" {
		t.Errorf("the store belongs to %s", got)
	}
}
