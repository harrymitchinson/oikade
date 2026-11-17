package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// fullStore is a closed store with enough in it to span several pages, and the size its newest meta
// page says it is.
func fullStore(t *testing.T) (dir string, size int64) {
	t.Helper()
	dir = t.TempDir()
	s := open(t, dir)
	n := s.Namespace("registry")
	for i := range 64 {
		if err := n.Put(fmt.Sprint(i), make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.View(func(tx *bolt.Tx) error { size = tx.Size(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, size
}

// A file bbolt would crash on, or couldn't read, is refused before bbolt maps it. A file cut short
// would otherwise fault the process, which nothing can recover from.
func TestOpenRefusesAFileItCantRead(t *testing.T) {
	page := int64(os.Getpagesize())
	for name, damage := range map[string]func(t *testing.T, path string, size int64){
		"cut by 3 pages": func(t *testing.T, path string, size int64) {
			t.Helper()
			if err := os.Truncate(path, size-3*page); err != nil {
				t.Fatal(err)
			}
		},
		"one page": func(t *testing.T, path string, _ int64) {
			t.Helper()
			if err := os.Truncate(path, page); err != nil {
				t.Fatal(err)
			}
		},
		"garbage": func(t *testing.T, path string, _ int64) {
			t.Helper()
			if err := os.WriteFile(path, []byte(strings.Repeat("not a store ", 8<<10)), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a few bytes": func(t *testing.T, path string, _ int64) {
			t.Helper()
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, size := fullStore(t)
			if size < 8*page {
				t.Fatalf("the store is only %d bytes", size)
			}
			damage(t, filepath.Join(dir, fileName), size)
			s, err := Open(dir)
			if err == nil {
				_ = s.Close()
			}
			if !errors.Is(err, ErrFormat) {
				t.Fatalf("Open = %v, want ErrFormat", err)
			}
		})
	}
}

// A store whose file is exactly as long as its meta page says opens, and so does an empty file,
// which is a new store.
func TestOpenTakesAWholeFileAndAnEmptyOne(t *testing.T) {
	dir, size := fullStore(t)
	if err := os.Truncate(filepath.Join(dir, fileName), size); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	if v, ok, err := s.Namespace("registry").Get("63"); err != nil || !ok || len(v) != 1024 {
		t.Errorf("the last value: %d bytes, %v, %v", len(v), ok, err)
	}

	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open(t, dir)
}

// countSyncs records the store's own syncs, by the name of what was synced.
func countSyncs(t *testing.T) *[]string {
	t.Helper()
	var synced []string
	before := syncFile
	syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		return before(f)
	}
	t.Cleanup(func() { syncFile = before })
	return &synced
}

// Open syncs the data directory, which holds the file's name, and its parent, which holds the
// directory's, on every open: a crash can come between creating them and the first write.
func TestOpenSyncsTheDataDirectoryAndItsParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	for range 2 {
		synced := countSyncs(t)
		s := open(t, dir)
		if want := []string{dir, filepath.Dir(dir)}; strings.Join(*synced, " ") != strings.Join(want, " ") {
			t.Errorf("Open synced %v, want %v", *synced, want)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A snapshot is synced before it takes its name, and its directory after, so a crash leaves either
// no snapshot or a whole one.
func TestSnapshotSyncsTheCopyThenItsDirectory(t *testing.T) {
	s := open(t, t.TempDir())
	out := t.TempDir()
	synced := countSyncs(t)
	if err := s.Snapshot(filepath.Join(out, "backup.db")); err != nil {
		t.Fatal(err)
	}
	got := *synced
	if len(got) != 2 || filepath.Dir(got[0]) != out || !strings.HasPrefix(filepath.Base(got[0]), ".snapshot-") || got[1] != out {
		t.Errorf("Snapshot synced %v, want the copy in %s, then %s", got, out, out)
	}
}
