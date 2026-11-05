// Package store is Oikade's one embedded store: a single bbolt file in a data directory, holding
// namespaced values and a write-ahead journal. Every write has reached disk when it returns.
// Nothing production uses it yet (OIK-143).
package store

import (
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
	// formatVersion is the layout of the buckets. Raise it when that layout changes.
	formatVersion = "1"
	// lockTimeout is how long Open waits for another process to let go of the data directory.
	lockTimeout = time.Second
)

// Top-level buckets. Namespaces are nested under kvBucket, so none can collide with the journal.
var (
	metaBucket    = []byte("meta")
	kvBucket      = []byte("kv")
	journalBucket = []byte("journal")
	formatKey     = []byte("format")
)

var (
	// ErrLocked means another process has the data directory open.
	ErrLocked = errors.New("data directory is in use by another process")
	// ErrFormat means the store was written by a version of Oikade with another layout.
	ErrFormat = errors.New("store has an unsupported format")
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

// init makes the file's name durable, and checks the format or writes it to a new store. The sync
// runs on every open, because a crash can come between the two.
func (s *Store) init(dir string) error {
	if err := syncDir(dir); err != nil {
		return err
	}
	var format []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(metaBucket); b != nil {
			format = b.Get(formatKey)
		}
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
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return b.Put(formatKey, []byte(formatVersion))
	}); err != nil {
		return fmt.Errorf("write store format: %w", err)
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
// share the store's file, and their keys don't collide.
func (s *Store) Namespace(name string) *Namespace {
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
		kv := tx.Bucket(kvBucket)
		if kv == nil {
			return nil
		}
		b := kv.Bucket(n.name)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(key)); v != nil {
			value = append(make([]byte, 0, len(v)), v...)
		}
		return nil
	})
	return value, value != nil, err
}

// Put sets the key's value, and returns once that's on disk.
func (n *Namespace) Put(key string, value []byte) error {
	return n.db.Update(func(tx *bolt.Tx) error {
		kv, err := tx.CreateBucketIfNotExists(kvBucket)
		if err != nil {
			return err
		}
		b, err := kv.CreateBucketIfNotExists(n.name)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), value)
	})
}

// Delete removes the key, if it's there, and returns once that's on disk.
func (n *Namespace) Delete(key string) error {
	return n.db.Update(func(tx *bolt.Tx) error {
		kv := tx.Bucket(kvBucket)
		if kv == nil {
			return nil
		}
		b := kv.Bucket(n.name)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}
