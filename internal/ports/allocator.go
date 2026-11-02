// Package ports allocates ports for host-networked bridges.
package ports

import "errors"

// ErrExhausted is returned when every port in the range is taken.
var ErrExhausted = errors.New("no free port in range")

// Allocate returns the lowest port from start to end, inclusive, that is not in used.
func Allocate(start, end int32, used map[int32]bool) (int32, error) {
	for p := start; p <= end; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, ErrExhausted
}
