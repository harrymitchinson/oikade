package store

import (
	"encoding/binary"
	"errors"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// ErrNoRun means a journal call named no run.
var ErrNoRun = errors.New("journal run has no ID")

// Entry is one record in a run's journal.
type Entry struct {
	// Seq is the entry's position in its run. It starts at 1 and only goes up.
	Seq  uint64
	Data []byte
}

// Journal is the write-ahead journal: an ordered list of entries for each run.
func (s *Store) Journal() *Journal {
	return &Journal{db: s.db}
}

// Journal keeps entries by run.
type Journal struct {
	db *bolt.DB
}

// Append adds an entry to the run, and returns only once it's on disk. An entry it returned for
// survives any crash. If it returns an error the entry may or may not be there, so a reader must
// accept either.
func (j *Journal) Append(run string, data []byte) (uint64, error) {
	if run == "" {
		return 0, ErrNoRun
	}
	var seq uint64
	err := j.db.Update(func(tx *bolt.Tx) error {
		runs, err := tx.CreateBucketIfNotExists(journalBucket)
		if err != nil {
			return err
		}
		b, err := runs.CreateBucketIfNotExists([]byte(run))
		if err != nil {
			return err
		}
		if seq, err = b.NextSequence(); err != nil {
			return err
		}
		return b.Put(binary.BigEndian.AppendUint64(nil, seq), data)
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// Entries returns the run's entries in the order they were appended. A run with none returns none.
func (j *Journal) Entries(run string) ([]Entry, error) {
	if run == "" {
		return nil, ErrNoRun
	}
	var entries []Entry
	err := j.db.View(func(tx *bolt.Tx) error {
		runs := tx.Bucket(journalBucket)
		if runs == nil {
			return nil
		}
		b := runs.Bucket([]byte(run))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			entries = append(entries, Entry{
				Seq:  binary.BigEndian.Uint64(k),
				Data: append(make([]byte, 0, len(v)), v...),
			})
			return nil
		})
	})
	return entries, err
}

// Runs returns every run with entries, which after a crash are the runs that hadn't finished.
func (j *Journal) Runs() ([]string, error) {
	var names []string
	err := j.db.View(func(tx *bolt.Tx) error {
		runs := tx.Bucket(journalBucket)
		if runs == nil {
			return nil
		}
		return runs.ForEachBucket(func(k []byte) error {
			names = append(names, string(k))
			return nil
		})
	})
	return names, err
}

// Delete removes a finished run's entries, and returns once that's on disk.
func (j *Journal) Delete(run string) error {
	if run == "" {
		return ErrNoRun
	}
	return j.db.Update(func(tx *bolt.Tx) error {
		runs := tx.Bucket(journalBucket)
		if runs == nil {
			return nil
		}
		err := runs.DeleteBucket([]byte(run))
		if errors.Is(err, bolterrors.ErrBucketNotFound) {
			return nil
		}
		return err
	})
}
