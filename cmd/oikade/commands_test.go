package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseTakesNegativeNumbersAndFlagsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"dev", "cap", "-5", "--json"},
		{"--json", "dev", "cap", "-5"},
		{"dev", "--json", "cap", "-5"},
	} {
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		got := parse(fs, args)
		if !slices.Equal(got, []string{"dev", "cap", "-5"}) || !*asJSON {
			t.Errorf("parse(%v) = %v, json %v", args, got, *asJSON)
		}
	}
	if v := parseValue("-5"); v != -5.0 {
		t.Errorf("got %v", v)
	}
}
