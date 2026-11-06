package simhome

import (
	"context"
	"slices"
	"sync"

	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// memory keeps states as the host would.
type memory[T any] struct {
	mu     sync.Mutex
	values map[ident.ID]T
}

func newMemory[T any]() *memory[T] {
	return &memory[T]{values: map[ident.ID]T{}}
}

func (m *memory[T]) Get(key ident.ID) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	return v, ok
}

func (m *memory[T]) Put(_ context.Context, key ident.ID, v T) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = v
	return nil
}

func (m *memory[T]) Delete(_ context.Context, key ident.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}

// journal keeps flow runs as the host would.
type journal struct {
	mu   sync.Mutex
	runs map[string][][]byte
}

func newJournal() *journal { return &journal{runs: map[string][][]byte{}} }

// Journal implements flow.Journal.
func (j *journal) Journal(_ context.Context, run string, data []byte) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.runs[run] = append(j.runs[run], slices.Clone(data))
	return uint64(len(j.runs[run])), nil
}

// Finish implements flow.Journal.
func (j *journal) Finish(_ context.Context, run string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.runs, run)
	return nil
}

func (j *journal) saved() []flow.Saved {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]flow.Saved, 0, len(j.runs))
	for id, entries := range j.runs {
		out = append(out, flow.Saved{ID: id, Entries: slices.Clone(entries)})
	}
	return out
}
