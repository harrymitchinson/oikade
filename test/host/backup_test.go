package host

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/matter/nodetest"
)

// copyDataDir copies what's in a data directory, as a backup tool does: it has no use for sockets.
func copyDataDir(t *testing.T, from string) string {
	t.Helper()
	to, err := os.MkdirTemp("", "oik") //nolint:usetesting // a Unix socket's path has to fit in about 100 bytes
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
	version := r.status().Home.Version
	if version == "" {
		t.Fatal("no version running")
	}
	// As a plugin's setup keeps a secret, which the copy of the directory takes with it.
	const kept = "a-device-key-from-setup"
	if err := os.WriteFile(filepath.Join(r.dir, "secrets", "kept"), []byte(kept), 0o600); err != nil {
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
	c.port = r.serveMatter(c.port, func(flag string) {
		started := r.count("Started host")
		r.launchWithout(flag, "--simulate", devicesFile(t))
		r.awaitCount(started+1, "Started host")
	})

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
	if got := r.status().Home.Version; got != version {
		t.Errorf("version %s became %s", version, got)
	}
	if got := r.keptSecret("kept"); got != kept {
		t.Errorf("the kept secret became %q", got)
	}
}
