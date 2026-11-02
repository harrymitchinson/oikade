package ports

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestAllocate(t *testing.T) {
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
			got, err := Allocate(100, 102, tt.used)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("Allocate() = %d, %v; want %d, %v", got, err, tt.want, tt.err)
			}
		})
	}
}

func TestReserver(t *testing.T) {
	var r Reserver
	a, b := types.NamespacedName{Name: "a"}, types.NamespacedName{Name: "b"}
	none := func() (map[int32]bool, error) { return nil, nil }
	if p, _ := r.Allocate(a, 100, 102, none); p != 100 {
		t.Errorf("a got %d", p)
	}
	// a's status isn't in the cache yet, but b mustn't take its port.
	if p, _ := r.Allocate(b, 100, 102, none); p != 101 {
		t.Errorf("b got %d", p)
	}
	if p, _ := r.Allocate(a, 100, 102, none); p != 100 {
		t.Errorf("a again got %d", p)
	}
	// A range that no longer holds a's port gives it a new one.
	if p, _ := r.Allocate(a, 200, 201, none); p != 200 {
		t.Errorf("a in a new range got %d", p)
	}
	r.Release(b)
	if p, _ := r.Allocate(a, 101, 101, none); p != 101 {
		t.Errorf("a after b's release got %d", p)
	}
}
