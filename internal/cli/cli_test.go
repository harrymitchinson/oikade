package cli

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
		got := Parse(fs, args)
		if !slices.Equal(got, []string{"dev", "cap", "-5"}) || !*asJSON {
			t.Errorf("Parse(%v) = %v, json %v", args, got, *asJSON)
		}
	}
}

func TestVersionPrefersTheStampedOne(t *testing.T) {
	if got := Version("v0.2.0"); got != "v0.2.0" {
		t.Errorf("got %s", got)
	}
}
