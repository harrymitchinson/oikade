// Package ports allocates HAP ports for host-networked bridges.
package ports

import (
	"errors"

	homekitv1alpha1 "github.com/harrymitchinson/oikade/api/homekit/v1alpha1"
)

// ErrExhausted is returned when every port in the range is taken.
var ErrExhausted = errors.New("no free port in range")

// Allocate returns the lowest port in r that is not in used.
func Allocate(r homekitv1alpha1.PortRange, used map[int32]bool) (int32, error) {
	for p := r.Start; p <= r.End; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, ErrExhausted
}
