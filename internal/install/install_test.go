package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

// machine is a systemd machine under a directory. A binary is a file holding its version, and one
// whose version says broken never serves.
type machine struct {
	t        *testing.T
	root     string
	users    map[string]bool
	running  string // the version systemd runs, or "" when it's stopped
	commands []string
	ufw      bool
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	return &machine{t: t, root: t.TempDir(), users: map[string]bool{}}
}

func (m *machine) run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.commands = append(m.commands, strings.Join(append([]string{name}, args...), " "))
	switch name {
	case "id":
		if !m.users[args[len(args)-1]] {
			return []byte("no such user"), errors.New("exit status 1")
		}
		return []byte("998"), nil
	case "useradd":
		m.users[args[len(args)-1]] = true
		return nil, nil
	case "ufw":
		if m.ufw {
			return []byte("Status: active\n"), nil
		}
		return []byte("Status: inactive\n"), nil
	case "systemctl":
		if args[0] == "restart" {
			if _, err := os.Stat(m.at(UnitPath)); err != nil {
				return nil, errors.New("Unit oikade.service not found")
			}
			m.running = m.read(BinPath)
		}
		return nil, nil
	case m.at(BinPath):
		return []byte(m.read(BinPath) + "\n"), nil
	}
	m.t.Fatalf("ran %s %v", name, args)
	return nil, nil
}

func (m *machine) call(_ context.Context, req api.Request) (api.Response, error) {
	if m.running == "" || strings.Contains(m.running, "broken") {
		return api.Response{}, syscall.ECONNREFUSED
	}
	switch req.Type {
	case api.TypeInfo:
		return api.Response{GOOS: "linux"}, nil
	case api.TypeBackup:
		m.commands = append(m.commands, "backup of "+m.running)
		return api.Response{Backup: "/var/lib/oikade/backups/oikade-20261120T100211Z.db"}, nil
	case api.TypeClientAdd:
		return api.Response{Code: "k7Qx", Pin: "sha256:9f2c", Address: "pi.home:8443",
			Expires: time.Now().Add(10 * time.Minute)}, nil
	}
	return api.Response{Error: "unexpected " + req.Type}, nil
}

func (m *machine) at(p string) string { return filepath.Join(m.root, p) }

func (m *machine) read(p string) string {
	m.t.Helper()
	data, err := os.ReadFile(m.at(p))
	if err != nil {
		m.t.Fatal(err)
	}
	return string(data)
}

// installer is version's binary, about to install itself on m.
func (m *machine) installer(version string) (*Installer, *bytes.Buffer) {
	m.t.Helper()
	self := filepath.Join(m.t.TempDir(), "oikade-host")
	if err := os.WriteFile(self, []byte(version), 0o755); err != nil {
		m.t.Fatal(err)
	}
	var progress bytes.Buffer
	return &Installer{
		Root: m.root, Self: self, Version: version, Privileged: true, Run: m.run, Call: m.call,
		Progress: &progress, Wait: 100 * time.Millisecond, Poll: 5 * time.Millisecond,
	}, &progress
}

var firstOptions = Options{APIAddr: DefaultAPIAddr, Client: DefaultClient}

func (m *machine) install(t *testing.T, version string, opts Options) (Result, string, error) {
	t.Helper()
	in, progress := m.installer(version)
	res, err := in.Install(t.Context(), opts)
	return res, progress.String(), err
}

func TestFirstInstall(t *testing.T) {
	m := newMachine(t)
	m.ufw = true
	res, progress, err := m.install(t, "v0.3.0", firstOptions)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.read(BinPath); got != "v0.3.0" {
		t.Errorf("binary is %q, not v0.3.0", got)
	}
	if info, err := os.Stat(m.at(BinPath)); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("binary's mode is %v, %v", info.Mode(), err)
	}
	if !m.users[User] {
		t.Error("made no user")
	}
	unit := m.read(UnitPath)
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/oikade-host --api-addr :8443\n") ||
		!strings.Contains(unit, "User=oikade\n") || !strings.Contains(unit, "KillMode=mixed\n") {
		t.Errorf("unit is\n%s", unit)
	}
	if m.running != "v0.3.0" {
		t.Errorf("running %q", m.running)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable oikade", "systemctl restart oikade"} {
		if !slices.Contains(m.commands, want) {
			t.Errorf("didn't run %s: %v", want, m.commands)
		}
	}
	if res.Upgraded || res.Login == nil || res.Login.Code != "k7Qx" {
		t.Errorf("result is %+v", res)
	}
	if res.FirewallRule != "sudo ufw allow 5540/udp && sudo ufw allow 5353/udp && sudo ufw allow 8443/tcp" {
		t.Errorf("firewall rule is %q", res.FirewallRule)
	}
	for _, want := range []string{"Installing oikade-host v0.3.0 to /usr/local/bin/oikade-host", "Created user oikade",
		"Wrote /etc/systemd/system/oikade.service, with the API on :8443", "Starting oikade, and waiting for it to serve"} {
		if !strings.Contains(progress, want) {
			t.Errorf("progress doesn't say %q:\n%s", want, progress)
		}
	}
}

func TestFirstInstallKeepsAUserThatsThere(t *testing.T) {
	m := newMachine(t)
	m.users[User] = true
	if _, progress, err := m.install(t, "v0.3.0", firstOptions); err != nil || strings.Contains(progress, "Created user") {
		t.Fatalf("%v:\n%s", err, progress)
	}
	if slices.ContainsFunc(m.commands, func(c string) bool { return strings.HasPrefix(c, "useradd") }) {
		t.Errorf("ran useradd: %v", m.commands)
	}
}

func TestFirstInstallThatNeverServes(t *testing.T) {
	m := newMachine(t)
	_, _, err := m.install(t, "v0.3.0 broken", firstOptions)
	if err == nil || !strings.Contains(err.Error(), "journalctl -u oikade") {
		t.Fatalf("error is %v", err)
	}
}

func TestUpgrade(t *testing.T) {
	m := newMachine(t)
	if _, _, err := m.install(t, "v0.3.0", firstOptions); err != nil {
		t.Fatal(err)
	}
	// A unit installed by hand, or edited, is kept: only flags write it again.
	edited := Unit(":9443") + "# mine\n"
	if err := os.WriteFile(m.at(UnitPath), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	m.commands = nil
	res, progress, err := m.install(t, "v0.3.1", Options{APIAddr: DefaultAPIAddr, Client: DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	if m.read(BinPath) != "v0.3.1" || m.read(PreviousPath) != "v0.3.0" || m.running != "v0.3.1" {
		t.Errorf("binary %q, previous %q, running %q", m.read(BinPath), m.read(PreviousPath), m.running)
	}
	if m.read(UnitPath) != edited {
		t.Errorf("unit changed to\n%s", m.read(UnitPath))
	}
	backup, restart := slices.Index(m.commands, "backup of v0.3.0"), slices.Index(m.commands, "systemctl restart oikade")
	if backup < 0 || restart < backup {
		t.Errorf("didn't back up before restarting: %v", m.commands)
	}
	if !res.Upgraded || res.From != "v0.3.0" || res.Previous != PreviousPath || res.Login != nil ||
		res.Backup == "" {
		t.Errorf("result is %+v", res)
	}
	if !strings.Contains(progress, "Upgrading oikade-host v0.3.0 to v0.3.1") ||
		!strings.Contains(progress, "Backed up the store to /var/lib/oikade/backups/") {
		t.Errorf("progress is\n%s", progress)
	}
}

func TestUpgradeWithFlags(t *testing.T) {
	m := newMachine(t)
	if _, _, err := m.install(t, "v0.3.0", firstOptions); err != nil {
		t.Fatal(err)
	}
	res, _, err := m.install(t, "v0.3.1",
		Options{APIAddr: ":9443", APIAddrSet: true, Client: "desktop", ClientSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.read(UnitPath), "--api-addr :9443\n") {
		t.Errorf("unit is\n%s", m.read(UnitPath))
	}
	if res.Login == nil {
		t.Error("no login for --client")
	}
}

func TestUpgradeThatDoesntServeRollsBack(t *testing.T) {
	m := newMachine(t)
	if _, _, err := m.install(t, "v0.3.0", firstOptions); err != nil {
		t.Fatal(err)
	}
	unit := m.read(UnitPath)
	_, _, err := m.install(t, "v0.3.1 broken", Options{APIAddr: ":9443", APIAddrSet: true})
	if err == nil || !strings.Contains(err.Error(), "so v0.3.0 is back") {
		t.Fatalf("error is %v", err)
	}
	if m.read(BinPath) != "v0.3.0" || m.running != "v0.3.0" || m.read(UnitPath) != unit {
		t.Errorf("binary %q, running %q, unit\n%s", m.read(BinPath), m.running, m.read(UnitPath))
	}
}

func TestUpgradeOfAHostThatIsntServing(t *testing.T) {
	m := newMachine(t)
	if _, _, err := m.install(t, "v0.3.0", firstOptions); err != nil {
		t.Fatal(err)
	}
	m.running = ""
	writeStore(t, m)
	_, _, err := m.install(t, "v0.3.1", Options{})
	if err == nil || !strings.Contains(err.Error(), "couldn't back up the store first") {
		t.Fatalf("error is %v", err)
	}
	if m.read(BinPath) != "v0.3.0" {
		t.Errorf("binary is %q", m.read(BinPath))
	}
}

func TestInstallOfTheBinaryThatsThere(t *testing.T) {
	m := newMachine(t)
	in, progress := m.installer("v0.3.0")
	if _, err := in.Install(t.Context(), firstOptions); err != nil {
		t.Fatal(err)
	}
	in.Self = m.at(BinPath)
	m.commands = nil
	res, err := in.Install(t.Context(), Options{})
	if err != nil || !res.Unchanged {
		t.Fatalf("result %+v, %v", res, err)
	}
	if slices.Contains(m.commands, "systemctl restart oikade") {
		t.Errorf("restarted: %v", m.commands)
	}
	if !strings.Contains(progress.String(), "oikade-host v0.3.0 is installed at /usr/local/bin/oikade-host already") {
		t.Errorf("progress is\n%s", progress)
	}
	if _, err := os.Stat(m.at(PreviousPath)); err == nil {
		t.Error("kept itself as the previous version")
	}
}

func TestInstallLeavesAUnitThatIsntOurs(t *testing.T) {
	m := newMachine(t)
	if err := os.MkdirAll(filepath.Dir(m.at(UnitPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	other := "[Service]\nExecStart=/opt/something/else\n"
	if err := os.WriteFile(m.at(UnitPath), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.install(t, "v0.3.0", firstOptions)
	if err == nil || !strings.Contains(err.Error(), "doesn't run /usr/local/bin/oikade-host") {
		t.Fatalf("error is %v", err)
	}
	if m.read(UnitPath) != other || len(m.commands) > 0 {
		t.Errorf("touched it: %v", m.commands)
	}
}

func TestInstallLeavesABinaryWithNoUnit(t *testing.T) {
	m := newMachine(t)
	if err := os.MkdirAll(filepath.Dir(m.at(BinPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.at(BinPath), []byte("v0.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.install(t, "v0.3.0", firstOptions)
	if err == nil || !strings.Contains(err.Error(), "may run from a unit install doesn't know") {
		t.Fatalf("error is %v", err)
	}
	if m.read(BinPath) != "v0.2.0" {
		t.Error("replaced the binary")
	}
}

func TestInstallNeedsRoot(t *testing.T) {
	m := newMachine(t)
	in, _ := m.installer("v0.3.0")
	in.Privileged = false
	if _, err := in.Install(t.Context(), firstOptions); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("error is %v", err)
	}
	if len(m.commands) > 0 {
		t.Errorf("ran %v", m.commands)
	}
}

func TestInstallRefusesAnAddressTheUnitCantCarry(t *testing.T) {
	m := newMachine(t)
	for _, addr := range []string{":8443 --data-dir /tmp", "%h:8443", ":8443;x"} {
		if _, _, err := m.install(t, "v0.3.0", Options{APIAddr: addr, APIAddrSet: true}); err == nil {
			t.Errorf("took %q", addr)
		}
	}
}

func TestRunsHost(t *testing.T) {
	for unit, want := range map[string]bool{
		Unit(""):                                 true,
		"ExecStart=-/usr/local/bin/oikade-host":  true,
		"ExecStart=/usr/bin/oikade-host":         false,
		"# ExecStart=/usr/local/bin/oikade-host": false,
	} {
		if got := runsHost([]byte(unit)); got != want {
			t.Errorf("runsHost(%q) = %v", unit, got)
		}
	}
}

func writeStore(t *testing.T, m *machine) {
	t.Helper()
	dir := m.at(cli.DefaultDataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.FileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A first install whose host never served can be run again, with a binary that does.
func TestInstallAgainAfterAFirstInstallThatFailed(t *testing.T) {
	m := newMachine(t)
	if _, _, err := m.install(t, "v0.3.0 broken", firstOptions); err == nil {
		t.Fatal("a host that never serves installed")
	}
	res, progress, err := m.install(t, "v0.3.0", firstOptions)
	if err != nil {
		t.Fatal(err)
	}
	if m.running != "v0.3.0" || res.Backup != "" || !strings.Contains(progress, "nothing to back up") {
		t.Errorf("running %q, result %+v:\n%s", m.running, res, progress)
	}
}

// A first install that stopped after the unit finishes as a first install.
func TestInstallWithAUnitAndNoBinary(t *testing.T) {
	m := newMachine(t)
	if err := os.MkdirAll(filepath.Dir(m.at(UnitPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.at(UnitPath), []byte(Unit(":9443")), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, err := m.install(t, "v0.3.0", firstOptions)
	if err != nil || res.Upgraded || m.running != "v0.3.0" {
		t.Fatalf("result %+v, running %q: %v", res, m.running, err)
	}
	if !strings.Contains(m.read(UnitPath), ":9443") {
		t.Error("wrote the unit again with no flags")
	}
}
