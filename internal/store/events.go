package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

// eventsBucket is created by the first Add, so a store from before the log needs nothing done to it.
var (
	eventsBucket = []byte("events")
	logBucket    = []byte("log")
	baselineKey  = []byte("baseline")
	sizeKey      = []byte("size")
)

const (
	// timeBytes is the event's time, at the start of each value.
	timeBytes = 8
	// fillPercent packs pages, because events are only ever added at the end.
	fillPercent = 0.9
	// minDrop is the least one Add may drop. An Add drops at most a few times what it adds, so that a
	// log far over a lowered bound shrinks over many short commits rather than one long one.
	minDrop    = 1024
	dropPerAdd = 4
	// keyBytes is a key's length, which the size counts.
	keyBytes = 8
)

// errNoEvents means nothing has been added yet.
var errNoEvents = errors.New("no event log")

// Record is one event in the log. The store doesn't read Data.
type Record struct {
	// Seq is its place in the log. It only rises, across drops and restarts.
	Seq  uint64
	Time time.Time
	Data []byte
}

// Bound is how much the log keeps. A zero field doesn't limit.
type Bound struct {
	MaxAge time.Duration
	// MaxBytes counts keys and values, not the file: bbolt's pages add to it, and the file doesn't
	// shrink, though it reuses what a drop frees.
	MaxBytes uint64
}

// Fold takes what's being dropped into the baseline, the summary of everything older than the log.
type Fold func(baseline []byte, dropped []Record) ([]byte, error)

// Events is a bounded log of what happened, kept for oikade replay. It's a history you can lose, not
// state: unlike the journal, nothing depends on it.
func (s *Store) Events() *Events {
	return &Events{db: s.db}
}

// Events keeps records in the order they were added, dropping the oldest.
type Events struct {
	db *bolt.DB
}

// Add appends records in one commit, and drops the oldest to stay within bound, folding them into the
// baseline in that same commit. Call it with no records to apply the bound alone. A record's Seq is
// set here.
func (e *Events) Add(records []Record, now time.Time, bound Bound, fold Fold) error {
	return e.db.Update(func(tx *bolt.Tx) error {
		root, err := tx.CreateBucketIfNotExists(eventsBucket)
		if err != nil {
			return err
		}
		log, err := root.CreateBucketIfNotExists(logBucket)
		if err != nil {
			return err
		}
		log.FillPercent = fillPercent
		size := decodeSize(root.Get(sizeKey))
		for _, r := range records {
			seq, err := log.NextSequence()
			if err != nil {
				return err
			}
			value := binary.BigEndian.AppendUint64(make([]byte, 0, timeBytes+len(r.Data)), uint64(r.Time.UnixNano()))
			value = append(value, r.Data...)
			if err := log.Put(binary.BigEndian.AppendUint64(nil, seq), value); err != nil {
				return err
			}
			size += keyBytes + uint64(len(value))
		}
		dropped, size := oldest(log, size, now, bound, max(minDrop, dropPerAdd*len(records)))
		if len(dropped) == 0 {
			return root.Put(sizeKey, binary.BigEndian.AppendUint64(nil, size))
		}
		baseline, err := fold(bytes.Clone(root.Get(baselineKey)), dropped)
		if err != nil {
			return err
		}
		if err := root.Put(baselineKey, baseline); err != nil {
			return err
		}
		for _, r := range dropped {
			if err := log.Delete(binary.BigEndian.AppendUint64(nil, r.Seq)); err != nil {
				return err
			}
		}
		return root.Put(sizeKey, binary.BigEndian.AppendUint64(nil, size))
	})
}

// oldest finds what's over the bound, up to limit records, and the size without them.
func oldest(log *bolt.Bucket, size uint64, now time.Time, bound Bound, limit int) ([]Record, uint64) {
	var cutoff time.Time
	if bound.MaxAge > 0 {
		cutoff = now.Add(-bound.MaxAge)
	}
	var out []Record
	c := log.Cursor()
	for k, v := c.First(); k != nil && len(out) < limit; k, v = c.Next() {
		r := record(k, v)
		if (bound.MaxBytes == 0 || size <= bound.MaxBytes) && !r.Time.Before(cutoff) {
			break
		}
		out = append(out, r)
		size -= uint64(len(k) + len(v))
	}
	return out, size
}

func record(k, v []byte) Record {
	return Record{
		Seq:  binary.BigEndian.Uint64(k),
		Time: time.Unix(0, int64(binary.BigEndian.Uint64(v))).UTC(), //nolint:gosec // Written from an int64.
		Data: bytes.Clone(v[timeBytes:]),
	}
}

func decodeSize(raw []byte) uint64 {
	if len(raw) != timeBytes {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

// Read returns up to limit records after seq, oldest first. Each call is a short transaction of its
// own: a long one would hold up the writes that grow the file, journal entries among them, so a
// reader of the whole log takes it in pieces.
func (e *Events) Read(after uint64, limit int) ([]Record, error) {
	var out []Record
	err := e.view(func(root, log *bolt.Bucket) error {
		c := log.Cursor()
		for k, v := c.Seek(binary.BigEndian.AppendUint64(nil, after+1)); k != nil && len(out) < limit; k, v = c.Next() {
			out = append(out, record(k, v))
		}
		return nil
	})
	return out, err
}

// Baseline is what the last Fold returned, or nil if nothing has been dropped.
func (e *Events) Baseline() ([]byte, error) {
	var out []byte
	err := e.view(func(root, _ *bolt.Bucket) error {
		out = bytes.Clone(root.Get(baselineKey))
		return nil
	})
	return out, err
}

// Size is how many bytes the log holds, as Bound counts them.
func (e *Events) Size() (uint64, error) {
	var out uint64
	err := e.view(func(root, _ *bolt.Bucket) error {
		out = decodeSize(root.Get(sizeKey))
		return nil
	})
	return out, err
}

func (e *Events) view(read func(root, log *bolt.Bucket) error) error {
	err := e.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(eventsBucket)
		if root == nil || root.Bucket(logBucket) == nil {
			return errNoEvents
		}
		return read(root, root.Bucket(logBucket))
	})
	if errors.Is(err, errNoEvents) {
		return nil
	}
	return err
}
