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

package ports

import (
	"errors"
	"testing"

	homekitv1alpha1 "github.com/harrymitchinson/oikade/api/homekit/v1alpha1"
)

func TestAllocate(t *testing.T) {
	r := homekitv1alpha1.PortRange{Start: 100, End: 102}
	tests := []struct {
		name string
		used map[int32]bool
		want int32
		err  error
	}{
		{"empty", nil, 100, nil},
		{"gap", map[int32]bool{100: true, 102: true}, 101, nil},
		{"outside range ignored", map[int32]bool{99: true, 100: true}, 101, nil},
		{"full", map[int32]bool{100: true, 101: true, 102: true}, 0, ErrExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Allocate(r, tt.used)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("Allocate() = %d, %v; want %d, %v", got, err, tt.want, tt.err)
			}
		})
	}
}
