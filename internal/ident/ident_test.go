package ident

import "testing"

func TestStringIsTheStoreKey(t *testing.T) {
	if got := (ID{Namespace: "home", Name: "fan"}).String(); got != "home/fan" {
		t.Fatalf("String() = %q", got)
	}
}
