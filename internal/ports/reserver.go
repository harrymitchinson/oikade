package ports

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// Reserver remembers the ports it handed out until their owners' status writes are visible in
// the cache, so two owners reconciled back to back can't get the same port.
type Reserver struct {
	mu       sync.Mutex
	reserved map[types.NamespacedName]int32
}

// Allocate returns a port from start to end for key: the one already reserved for it if that's
// still in range, or else the lowest one that isn't in used or reserved for another key. list
// returns the ports in use, other than key's, and is called with the Reserver locked.
func (r *Reserver) Allocate(key types.NamespacedName, start, end int32, list func() (map[int32]bool, error)) (int32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if port, ok := r.reserved[key]; ok && port >= start && port <= end {
		return port, nil
	}
	used, err := list()
	if err != nil {
		return 0, err
	}
	if used == nil {
		used = map[int32]bool{}
	}
	for k, port := range r.reserved {
		if k != key {
			used[port] = true
		}
	}
	port, err := Allocate(start, end, used)
	if err != nil {
		return 0, err
	}
	if r.reserved == nil {
		r.reserved = map[types.NamespacedName]int32{}
	}
	r.reserved[key] = port
	return port, nil
}

// Release forgets key's reservation, once its owner is gone.
func (r *Reserver) Release(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserved, key)
}
