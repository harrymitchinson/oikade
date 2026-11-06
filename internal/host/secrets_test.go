package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/logging"
)

func TestListSkipsWhatACrashLeft(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"api-key", ".new-123"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("value"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := secrets{dir: dir}.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "api-key" {
		t.Errorf("found %+v", found)
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

func TestSetOnAReadOnlyDirectorySaysWhereToChangeIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := secrets{dir: dir}.set("api-key", "value")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("err = %v", err)
	}
}

func TestAnAPIRefusalNeverEchoesTheValue(t *testing.T) {
	s := &apiServer{log: logging.Discard(), secrets: secrets{dir: filepath.Join(t.TempDir(), "gone")}}
	resp := s.secret(api.Request{Type: api.TypeSecretSet, Secret: "api-key", SecretValue: "the-value"})
	if resp.Error == "" || strings.Contains(resp.Error, "the-value") {
		t.Errorf("error = %q", resp.Error)
	}
	resp = s.secret(api.Request{Type: api.TypeSecretSet, Secret: "../x", SecretValue: "the-value"})
	if resp.Error == "" || strings.Contains(resp.Error, "the-value") {
		t.Errorf("error = %q", resp.Error)
	}
}

func TestBrokerPasswordComesFromTheSecretAndStaysOutOfTheLog(t *testing.T) {
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
