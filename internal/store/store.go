// Package store is Oikade's one embedded store: a single bbolt file in a data directory, holding
// namespaced values and a write-ahead journal. Every write has reached disk when it returns.
// The host owns it (OIK-143).
package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

const (
	// fileName is the store's file in the data directory.
	fileName = "oikade.db"
	// formatVersion is the layout of the buckets and the keys in them. Raise it when that changes.
	formatVersion = "2"
	// lockTimeout is how long Open waits for another process to let go of the data directory.
	lockTimeout = time.Second
)

// Top-level buckets. Namespaces are nested under kvBucket, so none can collide with the journal.
var (
	metaBucket    = []byte("meta")
	kvBucket      = []byte("kv")
	journalBucket = []byte("journal")
	// journalCopyBucket is the journal as it was before a deploy, while that deploy may yet fail.
	// It's there only then, so it needs no change to the format.
	journalCopyBucket = []byte("journal-copy")
	formatKey         = []byte("format")
)

var (
	// ErrLocked means another process has the data directory open.
	ErrLocked = errors.New("data directory is in use by another process")
	// ErrFormat means the file isn't a store this version of Oikade can read.
	ErrFormat = errors.New("store has an unsupported format")

	errNotEmpty = errors.New("store isn't empty")
)

// Store is an open data directory. It is safe for concurrent use.
type Store struct {
	db *bolt.DB
}

// Open opens the store in dir, creating both if needed. Only one process can have a data directory
// open: another gets ErrLocked. The operating system drops that lock when the process dies, however
// it dies, so a crash never leaves the directory locked.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	opts := *bolt.DefaultOptions
	opts.Timeout = lockTimeout
	path := filepath.Join(dir, fileName)
	db, err := bolt.Open(path, 0o600, &opts)
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("open %s: %w", dir, ErrLocked)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.init(dir); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

// init makes the file's name durable, and checks the format or sets up a new store. The syncs run
// on every open, because a crash can come between them and the setup. The parent is synced too, as
// it holds the data directory's own name.
func (s *Store) init(dir string) error {
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	var format []byte
	var empty bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(metaBucket); b != nil {
			format = bytes.Clone(b.Get(formatKey))
		}
		empty = tx.ForEach(func([]byte, *bolt.Bucket) error { return errNotEmpty }) == nil
		return nil
	}); err != nil {
		return fmt.Errorf("read store format: %w", err)
	}
	if format != nil {
		if string(format) != formatVersion {
			return fmt.Errorf("%w: %s, want %s", ErrFormat, format, formatVersion)
		}
		return nil
	}
	if !empty {
		return fmt.Errorf("%w: it has data but no format", ErrFormat)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{kvBucket, journalBucket} {
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		meta, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		return meta.Put(formatKey, []byte(formatVersion))
	}); err != nil {
		return fmt.Errorf("set up store: %w", err)
	}
	return nil
}

// syncDir makes a new file in dir durable. Syncing a file doesn't sync its directory entry.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open data directory: %w", err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync data directory: %w", err), d.Close())
	}
	return d.Close()
}

// Close closes the store and releases the data directory.
func (s *Store) Close() error {
	return s.db.Close()
}

// Namespace returns the named set of values, such as a device registry or saved holds. Namespaces
// share the store's file, and their keys don't collide. Names are constants in the code, so an
// empty one is a bug, and it panics.
func (s *Store) Namespace(name string) *Namespace {
	if name == "" {
		panic("store: namespace has no name")
	}
	return &Namespace{db: s.db, name: []byte(name)}
}

// Namespace is a set of values in the store.
type Namespace struct {
	db   *bolt.DB
	name []byte
}

// Get returns a copy of the key's value, if there is one.
func (n *Namespace) Get(key string) ([]byte, bool, error) {
	var value []byte
	err := n.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(kvBucket).Bucket(n.name); b != nil {
			value = bytes.Clone(b.Get([]byte(key)))
		}
		return nil
	})
	return value, value != nil, err
}

// Keys returns every key in the namespace, in order.
func (n *Namespace) Keys() ([]string, error) {
	var keys []string
	err := n.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(kvBucket).Bucket(n.name)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	return keys, err
}

// Put sets the key's value, and returns once that's on disk.
func (n *Namespace) Put(key string, value []byte) error {
	return n.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(kvBucket).CreateBucketIfNotExists(n.name)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), value)
	})
}

// Delete removes the key, if it's there, and returns once that's on disk.
func (n *Namespace) Delete(key string) error {
	return n.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(kvBucket).Bucket(n.name)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

// Snapshot writes a consistent copy of the store to path, without stopping anything that's using
// it, and returns once the copy has reached disk. A copy of the file itself could tear: this reads
// one transaction, so it's one moment of the store, and it opens like the original.
func (s *Store) Snapshot(path string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	err = s.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(f)
		return err
	})
	if err == nil {
		err = f.Sync()
	}
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("finish snapshot: %w", err)
	}
	return syncDir(dir)
}
