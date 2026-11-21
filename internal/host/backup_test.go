package host

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"oikade.dev/internal/logging"
	"oikade.dev/internal/store"
)

// A backup keeps the newest snapshots, its own among them, and leaves anything else in the
// directory alone.
func TestABackupKeepsTheNewestAndOnlyItsOwn(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	backups := filepath.Join(dir, BackupsDir)
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	old := make([]string, 0, keptBackups+1)
	for day := range keptBackups + 1 {
		old = append(old, BackupName(time.Date(2020, 1, 1+day, 0, 0, 0, 0, time.UTC)))
	}
	for _, name := range append(slices.Clone(old), "notes.txt") {
		if err := os.WriteFile(filepath.Join(backups, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &apiServer{log: logging.Discard(), data: data, dataDir: dir}
	resp := s.backup()
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	// The two oldest go, to keep the newest four and this one.
	want := append(slices.Clone(old[2:]), filepath.Base(resp.Backup), "notes.txt")
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
	if info, err := os.Stat(resp.Backup); err != nil || info.Size() == 0 {
		t.Errorf("the new backup: %v, %v", info, err)
	}
}
