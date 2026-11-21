package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A setup can keep a device's key under the device's ID, which may start with a digit, and a name
// that would leave the directory is refused.
func TestASetupKeepsASecretUnderAPluginsName(t *testing.T) {
	s := secrets{dir: t.TempDir()}
	if err := s.set("1000aabbcc", "v"); err != nil {
		t.Error(err)
	}
	for _, name := range []string{"../x", "a/b", ""} {
		if err := s.set(name, "the-value"); err == nil || strings.Contains(err.Error(), "the-value") {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestSetLeavesOnlyTheFileAndReplacesIt(t *testing.T) {
	s := secrets{dir: t.TempDir()}
	for _, value := range []string{"first", "second"} {
		if err := s.set("api-key", value); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, "api-key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "second" {
		t.Errorf("value = %q", raw)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left %v", entries)
	}
}

func TestBrokerPasswordComesFromItsFileAndStaysOutOfTheLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broker"), []byte("p@ss/word\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := WithBrokerPassword("mqtt://oikade@broker:1883", dir, "broker")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "mqtt://oikade:") || !strings.HasSuffix(got, "@broker:1883") {
		t.Errorf("url = %s", got)
	}
	if shown := redacted(got); strings.Contains(shown, "word") {
		t.Errorf("redacted = %s", shown)
	}
	if shown := redacted("mqtt://a:secret@bad host"); strings.Contains(shown, "secret") {
		t.Errorf("redacted = %s", shown)
	}
	if _, err := WithBrokerPassword("mqtt://broker:1883", dir, "broker"); err == nil {
		t.Error("no user to give the password to")
	}
}

func TestBackupNamesSortByAge(t *testing.T) {
	names := []string{BackupName(mustTime(t, "2026-11-06T10:00:00Z")), BackupName(mustTime(t, "2026-12-01T09:00:00Z"))}
	if !slices.IsSorted(names) {
		t.Errorf("%v", names)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
