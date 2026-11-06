package host

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
)

const secretValue = "correct-horse-battery-staple"

// setSecret sets a secret as a script does, with its value on stdin.
func (r *rig) setSecret(name string) error {
	r.t.Helper()
	cmd := exec.Command(hostBin, "secret", "set", "--data-dir", r.dir, name)
	cmd.Stdin = strings.NewReader(secretValue + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return &exec.ExitError{Stderr: out.Bytes()}
	}
	if strings.Contains(out.String(), secretValue) {
		r.t.Errorf("oikade secret set printed the value: %s", out.String())
	}
	return nil
}

// copyDataDir copies what's in a data directory, as a backup tool does: it has no use for sockets.
func copyDataDir(t *testing.T, from string) string {
	t.Helper()
	to, err := os.MkdirTemp("", "oik")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(to) })
	err = filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(filepath.Join(to, rel), info.Mode().Perm())
		case !info.Mode().IsRegular():
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// A snapshot of a running host, taken with the directory, is all a new machine needs.
func TestARestoredDataDirectoryBringsTheHomeBackWithNoPairing(t *testing.T) {
	t.Parallel()
	c := commissioned(t)
	r := c.r
	if stderr, err := r.deploy("backup"); err != nil {
		t.Fatalf("deploying: %v: %s", err, stderr)
	}
	before, endpoint := r.matter(), c.endpoint("fan")
	unique := c.info(endpoint, uniqueIDAttribute)
	version := r.status().Version
	if version == "" {
		t.Fatal("no version running")
	}
	if err := r.setSecret("api-key"); err != nil {
		t.Fatal(err)
	}
	backup := strings.TrimSpace(string(r.stdout(nil, "backup")))
	if filepath.Dir(backup) != filepath.Join(r.dir, "backups") {
		t.Fatalf("the backup is at %s", backup)
	}
	copied := copyDataDir(t, r.dir)

	// A new machine: the old host has gone, and it has the snapshot in the place of the live file,
	// which was changing as it was copied.
	if err := r.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	r.died()
	snapshot := filepath.Join(copied, "backups", filepath.Base(backup))
	raw, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copied, "oikade.db"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r.dir = copied
	r.launchWithout(matterFlag(c.port), "--simulate", devicesFile(t))
	r.awaitCount(2, "Started host")

	var after api.Matter
	r.until("the fabric", func() bool {
		after = r.matter()
		return len(after.Fabrics) == 1
	})
	if after.Window || after.ManualCode != before.ManualCode || after.QRCode != before.QRCode {
		t.Errorf("before %+v, after %+v", before, after)
	}
	// The controller paired with the old machine talks to this one, having paired with nothing.
	c.admin = c.cm.CASE(nodetest.Addr(c.port))
	if got := c.endpoint("fan"); got != endpoint {
		t.Errorf("fan moved from endpoint %d to %d", endpoint, got)
	}
	if got := c.info(endpoint, uniqueIDAttribute); got != unique {
		t.Errorf("unique ID %v became %v", unique, got)
	}
	c.reaches("fan")
	if got := r.status().Version; got != version {
		t.Errorf("version %s became %s", version, got)
	}
	if !strings.Contains(string(r.stdout(nil, "secret", "list")), "api-key") {
		t.Error("the secret didn't come back")
	}
}

func TestBackupsAreBoundedAndNewestKept(t *testing.T) {
	t.Parallel()
	r := start(t, "--matter-port=0")
	dir := filepath.Join(r.dir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"20200101", "20200102", "20200103", "20200104", "20200105", "20200106"} {
		if err := os.WriteFile(filepath.Join(dir, "oikade-"+old+"T000000Z.db"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newest := strings.TrimSpace(string(r.stdout(nil, "backup")))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Errorf("%d backups kept", len(entries))
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("the newest went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "oikade-20200101T000000Z.db")); err == nil {
		t.Error("the oldest stayed")
	}
}

func TestSecretsReachYourProcessAndPluginsAndNoLogOrAnswer(t *testing.T) {
	t.Parallel()
	r := prepare(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.launch("--matter-port=0")
	r.waitFor(func(l line) bool { return l.is(attached) })
	if err := r.setSecret("fixture-secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(r.dir, "secrets", "fixture-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the secret's file is %o", info.Mode().Perm())
	}

	// The next process finds it, as a file, and says only how long it was.
	r.kill()
	r.awaitCount(1, "Read secret", "length", float64(len(secretValue)))

	var listed []api.Secret
	if err := json.Unmarshal(r.stdout(nil, "secret", "list", "--json"), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "fixture-secret" || listed[0].Changed.IsZero() {
		t.Errorf("listed %+v", listed)
	}
	if out := r.stdout(nil, "secret", "list", "--json"); bytes.Contains(out, []byte(secretValue)) {
		t.Errorf("--json has the value: %s", out)
	}
	r.stdout(nil, "secret", "remove", "fixture-secret")
	if out := r.stdout(nil, "secret", "list"); len(out) != 0 {
		t.Errorf("after removing it: %s", out)
	}
	if strings.Contains(r.dump(), secretValue) {
		t.Errorf("the value is in the host's log:\n%s", r.dump())
	}
}

func TestASecretsNameCantLeaveTheDirectory(t *testing.T) {
	t.Parallel()
	r := start(t, "--matter-port=0")
	for _, name := range []string{"../oikade.db", ".hidden", "a/b", ""} {
		if err := r.setSecret(name); err == nil {
			t.Errorf("set a secret called %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join(r.dir, "oikade.db")); err != nil {
		t.Fatal(err)
	}
}

// oikade secret set needs a running host, so a host can't refuse to start for want of a secret.
func TestAHostStartsBeforeItsBrokerPasswordIsSet(t *testing.T) {
	t.Parallel()
	r := start(t, "--broker", "mqtt://oikade@127.0.0.1:1", "--broker-password-secret", "broker")
	if err := r.setSecret("broker"); err != nil {
		t.Fatal(err)
	}
	if r.count("Could not read the broker's password; restart the host once it's set") != 1 {
		t.Errorf("it didn't say why it has no password:\n%s", r.dump())
	}
}
