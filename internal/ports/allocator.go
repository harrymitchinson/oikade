/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ports allocates HAP ports for host-networked bridges.
package ports

import (
	"errors"

	homekitv1alpha1 "github.com/harry/oikade/api/homekit/v1alpha1"
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
