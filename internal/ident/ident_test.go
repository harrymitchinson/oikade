package ident

import "testing"

func TestStringIsTheStoreKey(t *testing.T) {
	if got := (ID{Name: "fan"}).String(); got != "fan" {
		t.Fatalf("String() = %q", got)
	}
}
