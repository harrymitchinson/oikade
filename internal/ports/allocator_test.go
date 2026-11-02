package ports

import (
	"errors"
	"testing"
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
