package main

import (
	"flag"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

func TestParseValueTakesNegativeNumbers(t *testing.T) {
	for _, args := range [][]string{
		{"dev", "cap", "-5", "--json"},
		{"--json", "dev", "cap", "-5"},
		{"dev", "--json", "cap", "-5"},
	} {
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		got := cli.Parse(fs, args)
		if !slices.Equal(got, []string{"dev", "cap", "-5"}) || !*asJSON {
			t.Errorf("cli.Parse(%v) = %v, json %v", args, got, *asJSON)
		}
	}
	if v := parseValue("-5"); v != -5.0 {
		t.Errorf("got %v", v)
	}
}

func TestSetSaysWhatTheDeviceWasLeftAt(t *testing.T) {
	expires := time.Date(2026, 11, 8, 17, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		c    *api.Capability
		want string
	}{
		{"unarbitrated", &api.Capability{Name: "on", Value: true}, "desk-plug on is true"},
		{"held", &api.Capability{Name: "on", Value: false, Level: "Safety", Claims: []api.Claim{
			{Source: "Rule/hall-too-humid", Level: "Safety", Value: false, Winner: true},
		}}, "desk-plug on is false, not true: Rule/hall-too-humid holds it at Safety"},
		{"for a while", &api.Capability{Name: "on", Value: true, Level: "Override", Claims: []api.Claim{
			{Source: "Command", Level: "Override", Value: true, Winner: true, Expires: &expires},
		}}, "desk-plug on is true: Command holds it at Override until 17:30"},
		{"written back", &api.Capability{Name: "on", Value: false, Level: "Default"},
			"desk-plug on is false, not true: nothing claims it, so its Default wins"},
	} {
		if got := outcome("desk-plug", "on", true, tc.c); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSetOnADeviceThatHasntAnsweredSaysSoAndPasses(t *testing.T) {
	got := outcome("desk-plug", "on", true, nil)
	if !strings.Contains(got, "desk-plug hasn't answered yet") {
		t.Errorf("got %q", got)
	}
	if err := done(api.Response{}, false, got); err != nil {
		t.Errorf("a set the device hasn't answered failed: %v", err)
	}
}

func TestPauseNamesTheClaimsItEnded(t *testing.T) {
	got := paused("Paused", "hall-fan", api.Response{Ended: []string{"fan on", "lamp on"}, Kept: []string{"valve on"}})
	want := "Paused hall-fan, and ended its claims on fan on, lamp on. " +
		"It keeps its Safety claims on valve on, which only a release ends"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := paused("Paused", "hall-fan", api.Response{}); got != "Paused hall-fan. It had no claims to end" {
		t.Errorf("with no claims: %q", got)
	}
}
