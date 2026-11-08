package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNamespaceKeepsValuesApartAndAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	registry, holds := s.Namespace("registry"), s.Namespace("holds")
	if err := registry.Put("a", []byte("device")); err != nil {
		t.Fatal(err)
	}
	if err := holds.Put("a", []byte("hold")); err != nil {
		t.Fatal(err)
	}
	if err := holds.Put("gone", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := holds.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir)
	if v, ok, err := s.Namespace("registry").Get("a"); err != nil || !ok || string(v) != "device" {
		t.Errorf("registry a = %q, %v, %v", v, ok, err)
	}
	if v, ok, err := s.Namespace("holds").Get("a"); err != nil || !ok || string(v) != "hold" {
		t.Errorf("holds a = %q, %v, %v", v, ok, err)
	}
	if _, ok, err := s.Namespace("holds").Get("gone"); err != nil || ok {
		t.Errorf("deleted key: ok = %v, err = %v", ok, err)
	}
}

func TestNamespaceMissingValues(t *testing.T) {
	s := open(t, t.TempDir())
	n := s.Namespace("nothing-here")
	if _, ok, err := n.Get("a"); err != nil || ok {
		t.Errorf("Get: ok = %v, err = %v", ok, err)
	}
	if err := n.Delete("a"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestNamespaceEmptyValueExists(t *testing.T) {
	s := open(t, t.TempDir())
	n := s.Namespace("n")
	if err := n.Put("a", []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := n.Get("a"); err != nil || !ok {
		t.Errorf("Get: ok = %v, err = %v", ok, err)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	s := open(t, t.TempDir())
	n := s.Namespace("n")
	if err := n.Put("a", []byte("value")); err != nil {
		t.Fatal(err)
	}
	v, _, err := n.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	v[0] = 'X' // bbolt's own memory is read-only, so this faults unless it's a copy.
	if v, _, _ := n.Get("a"); string(v) != "value" {
		t.Errorf("value = %q", v)
	}
}

func TestOpenRefusesAnotherFormat(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucket).Put(formatKey, []byte("99"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrFormat) {
		t.Fatalf("Open = %v, want ErrFormat", err)
	}
	// The refused Open let go of the directory.
	s, err := Open(dir)
	if s != nil {
		_ = s.Close()
	}
	if errors.Is(err, ErrLocked) {
		t.Fatal("Open left the data directory locked")
	}
}

// Format 1 kept saved state and driver names under a namespace that no longer exists.
func TestOpenRefusesTheNamespacedFormat(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucket).Put(formatKey, []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrFormat) {
		t.Fatalf("Open = %v, want ErrFormat", err)
	}
}

func TestOpenRefusesAFileThatIsntAStore(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(metaBucket) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrFormat) {
		t.Fatalf("Open = %v, want ErrFormat", err)
	}
}

func TestWritesAreSynced(t *testing.T) {
	// Nothing else here would notice: SIGKILL leaves the page cache alone.
	if s := open(t, t.TempDir()); s.db.NoSync {
		t.Fatal("bbolt is set not to sync")
	}
}

func TestNamespaceNeedsAName(t *testing.T) {
	s := open(t, t.TempDir())
	defer func() {
		if recover() == nil {
			t.Error("an empty namespace didn't panic")
		}
	}()
	s.Namespace("")
}

func TestJournalKeepsRunsInOrder(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	j := s.Journal()
	for _, e := range []struct{ run, data string }{{"a", "1"}, {"b", "x"}, {"a", "2"}, {"a", "3"}} {
		if _, err := j.Append(e.run, []byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	j = open(t, dir).Journal()
	got, err := j.Entries("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %v", got)
	}
	for i, want := range []string{"1", "2", "3"} {
		if got[i].Seq != uint64(i+1) || !bytes.Equal(got[i].Data, []byte(want)) {
			t.Errorf("entry %d = %+v", i, got[i])
		}
	}
	runs, err := j.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0] != "a" || runs[1] != "b" {
		t.Errorf("runs = %v", runs)
	}
}

func TestJournalOrdersPastByteBoundaries(t *testing.T) {
	j := open(t, t.TempDir()).Journal()
	for range 300 {
		if _, err := j.Append("a", nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := j.Entries("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 300 {
		t.Fatalf("got %d entries", len(got))
	}
	for i, e := range got {
		if e.Seq != uint64(i+1) {
			t.Fatalf("entry %d has seq %d", i, e.Seq)
		}
	}
}

func TestJournalDeletesAFinishedRun(t *testing.T) {
	j := open(t, t.TempDir()).Journal()
	if _, err := j.Append("done", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append("waiting", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := j.Delete("done"); err != nil {
		t.Fatal(err)
	}
	if err := j.Delete("never-existed"); err != nil {
		t.Errorf("Delete of an unknown run: %v", err)
	}
	if got, err := j.Entries("done"); err != nil || len(got) != 0 {
		t.Errorf("entries = %v, %v", got, err)
	}
	if runs, err := j.Runs(); err != nil || len(runs) != 1 || runs[0] != "waiting" {
		t.Errorf("runs = %v, %v", runs, err)
	}
}

func TestJournalNeedsARun(t *testing.T) {
	j := open(t, t.TempDir()).Journal()
	if _, err := j.Append("", nil); !errors.Is(err, ErrNoRun) {
		t.Errorf("Append: %v", err)
	}
	if _, err := j.Entries(""); !errors.Is(err, ErrNoRun) {
		t.Errorf("Entries: %v", err)
	}
	if err := j.Delete(""); !errors.Is(err, ErrNoRun) {
		t.Errorf("Delete: %v", err)
	}
}

func BenchmarkJournalAppend(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	j := s.Journal()
	data := bytes.Repeat([]byte("x"), 256)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := j.Append("run", data); err != nil {
			b.Fatal(err)
		}
	}
}

func TestKeysListsANamespace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	n := s.Namespace("things")
	if keys, err := n.Keys(); err != nil || len(keys) != 0 {
		t.Fatalf("got %v, %v before anything was written", keys, err)
	}
	for _, k := range []string{"b", "a"} {
		if err := n.Put(k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Namespace("others").Put("c", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if keys, err := n.Keys(); err != nil || len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("got %v, %v", keys, err)
	}
}

func TestSnapshotOpensWithWhatTheStoreHeld(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ns := s.Namespace("things")
	if err := ns.Put("kept", []byte("yes")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Journal().Append("run-1", []byte("step")); err != nil {
		t.Fatal(err)
	}
	copied := t.TempDir()
	if err := s.Snapshot(filepath.Join(copied, fileName)); err != nil {
		t.Fatal(err)
	}
	// Nothing written after the snapshot is in it, and the store it came from is still open.
	if err := ns.Put("later", []byte("no")); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(copied)
	if err != nil {
		t.Fatalf("opening the snapshot: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if got, ok, err := restored.Namespace("things").Get("kept"); err != nil || !ok || string(got) != "yes" {
		t.Errorf("kept = %q, %v, %v", got, ok, err)
	}
	if _, ok, err := restored.Namespace("things").Get("later"); err != nil || ok {
		t.Errorf("a write after the snapshot is in it: %v, %v", ok, err)
	}
	if entries, err := restored.Journal().Entries("run-1"); err != nil || len(entries) != 1 {
		t.Errorf("journal = %v, %v", entries, err)
	}
	if left, err := os.ReadDir(copied); err != nil || len(left) != 1 {
		t.Errorf("the snapshot's directory holds %v, %v", left, err)
	}
}
