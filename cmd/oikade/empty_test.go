package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
)

func TestEmptyListsSayWhatWouldFillThem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		print func(stdout, stderr *bytes.Buffer, asJSON bool) error
		hint  string
	}{
		{"secrets", func(stdout, stderr *bytes.Buffer, asJSON bool) error {
			return printSecrets(stdout, stderr, nil, asJSON)
		}, "No secrets. oikade secret set NAME adds one."},
		{"clients", func(stdout, stderr *bytes.Buffer, asJSON bool) error {
			return printClients(stdout, stderr, nil, asJSON)
		}, "No clients. oikade-host client add NAME, on the host's machine, adds one."},
	} {
		var stdout, stderr bytes.Buffer
		if err := tc.print(&stdout, &stderr, false); err != nil {
			t.Fatal(err)
		}
		if stdout.Len() != 0 || strings.TrimSpace(stderr.String()) != tc.hint {
			t.Errorf("%s: stdout %q, stderr %q", tc.name, stdout.String(), stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		if err := tc.print(&stdout, &stderr, true); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "[]\n" || stderr.Len() != 0 {
			t.Errorf("%s --json: stdout %q, stderr %q", tc.name, stdout.String(), stderr.String())
		}
	}
}

func TestNoDevicesSaysWhatReportsThem(t *testing.T) {
	var stderr bytes.Buffer
	noDevices(&stderr, 0)
	if !strings.Contains(stderr.String(), "A plugin your home imports reports its devices") ||
		strings.Contains(stderr.String(), "--all") {
		t.Errorf("got %q", stderr.String())
	}
	stderr.Reset()
	noDevices(&stderr, 2)
	got := stderr.String()
	if !strings.Contains(got, "2 removed devices") || !strings.Contains(got, "oikade devices --all") {
		t.Errorf("with removed devices: %q", stderr.String())
	}
	raw, err := json.Marshal(listed(registry.Snapshot{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"devices":[]`) {
		t.Errorf("--json: %s", raw)
	}
}

func TestStatusWithNothingDeployedNamesDeploy(t *testing.T) {
	var stdout, stderr bytes.Buffer
	printStatus(&stdout, &stderr, &api.Status{}, true, time.Now())
	if !strings.Contains(stderr.String(), "No home deployed. Run oikade deploy from your home's module.") {
		t.Errorf("stderr %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "unknown") {
		t.Errorf("stdout %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	printStatus(&stdout, &stderr, &api.Status{Deploying: true}, true, time.Now())
	if !strings.Contains(stdout.String(), "while a deploy is under way") || stderr.Len() != 0 {
		t.Errorf("during a deploy: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestStatusSaysSinceWhenAutomationsAreDown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	at := time.Now().Add(-time.Minute)
	printStatus(&stdout, &stderr, &api.Status{Version: "v3", Since: &at}, true, time.Now())
	want := "Version v3, automations down" + since(&at, time.Now())
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout %q, want %q", stdout.String(), want)
	}
	if got := since(&at, at); got != " since "+at.Local().Format("15:04") {
		t.Errorf("today: %q", got)
	}
	if !strings.Contains(stderr.String(), "oikade tail") {
		t.Errorf("stderr %q", stderr.String())
	}
	if got := since(&at, at.AddDate(0, 0, 1)); got != " since "+at.Local().Format(time.DateTime) {
		t.Errorf("a day later: %q", got)
	}
}
