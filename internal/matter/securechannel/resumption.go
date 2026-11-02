package securechannel

import (
	"bytes"
	"slices"
	"sync"
)

// DefaultResumptions is how many peers a ResumptionStore remembers if it's given no limit.
const DefaultResumptions = 64

// ResumptionStore remembers, in memory, what a node needs to resume CASE sessions with the peers
// it recently established one with (Core spec, 4.14.2.2): one entry per peer, the oldest dropped
// first once it's full. A responder looks entries up by resumption ID, and an initiator by peer.
// Both sides keep their own store. It's safe for concurrent use, and a nil store remembers
// nothing.
type ResumptionStore struct {
	max int

	mu      sync.Mutex
	entries []resumption // oldest first
}

type resumption struct {
	id     [resumptionIDSize]byte
	fabric uint8
	// rootKey and fabricID are the fabric's, so a resumption can't carry over to another fabric
	// that takes the same index.
	rootKey  []byte
	fabricID uint64
	node     uint64
	cats     []uint32
	secret   []byte // the ECDH shared secret of the CASE that this resumes
}

// NewResumptionStore returns a store that remembers at most max peers, or DefaultResumptions if
// max isn't positive.
func NewResumptionStore(max int) *ResumptionStore {
	if max <= 0 {
		max = DefaultResumptions
	}
	return &ResumptionStore{max: max}
}

// save remembers a resumption, replacing the peer's earlier one.
func (s *ResumptionStore) save(r resumption) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = slices.DeleteFunc(s.entries, func(e resumption) bool {
		return e.fabric == r.fabric && e.node == r.node || e.id == r.id
	})
	if len(s.entries) >= s.max {
		s.entries = slices.Delete(s.entries, 0, len(s.entries)-s.max+1)
	}
	s.entries = append(s.entries, r)
}

// on reports whether the resumption is on the fabric f.
func (r resumption) on(f *fabricKeys) bool {
	return r.fabric == f.Index && r.fabricID == f.fabricID && bytes.Equal(r.rootKey, f.rcac.PublicKey)
}

// take removes and returns the resumption with the given ID, if ok accepts it.
func (s *ResumptionStore) take(id []byte, ok func(resumption) bool) (resumption, bool) {
	if s == nil {
		return resumption{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if string(e.id[:]) == string(id) {
			if !ok(e) {
				return resumption{}, false
			}
			s.entries = slices.Delete(s.entries, i, i+1)
			return e, true
		}
	}
	return resumption{}, false
}

func (s *ResumptionStore) byPeer(fabric uint8, node uint64) (resumption, bool) {
	if s == nil {
		return resumption{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.fabric == fabric && e.node == node {
			return e, true
		}
	}
	return resumption{}, false
}

// forget drops a peer's resumption, after it failed.
func (s *ResumptionStore) forget(fabric uint8, node uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = slices.DeleteFunc(s.entries, func(e resumption) bool { return e.fabric == fabric && e.node == node })
}

// RemoveFabric forgets every resumption on a fabric, for when the fabric is removed or its index
// reused.
func (s *ResumptionStore) RemoveFabric(fabric uint8) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = slices.DeleteFunc(s.entries, func(e resumption) bool { return e.fabric == fabric })
}

// Len returns how many peers the store remembers.
func (s *ResumptionStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
