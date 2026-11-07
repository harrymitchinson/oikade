package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

func TestConfigIsPrivate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := clientConfig{Current: "pi", Contexts: map[string]hostContext{
		"pi": {Address: "pi:8443", Pin: "sha256:00", Token: "oik_x"},
	}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	path, _ := configPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the config's mode is %o", mode)
	}
	if got, err := loadConfig(); err != nil || got.Contexts["pi"].Token != "oik_x" {
		t.Errorf("reading it back: %+v, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a config others can read: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*")); len(leftovers) != 0 {
		t.Errorf("left %v behind", leftovers)
	}
}

func TestTargetChoosesTheHost(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(tokenEnv, "")
	pick := func(args ...string) api.Transport {
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		target := addTarget(fs)
		parse(fs, args)
		return target.transport()
	}
	if _, ok := pick().(api.Local); !ok {
		t.Error("with no config, a command should use this machine")
	}
	if err := saveConfig(clientConfig{Current: "pi", Contexts: map[string]hostContext{
		"pi":  {Address: "pi:8443", Pin: "sha256:01", Token: "oik_pi"},
		"lab": {Address: "lab:8443", Pin: "sha256:02"},
	}}); err != nil {
		t.Fatal(err)
	}
	if r, ok := pick().(api.Remote); !ok || r.Address != "pi:8443" || r.Token != "oik_pi" {
		t.Errorf("the current context: %#v", pick())
	}
	if r, ok := pick("--context", "lab").(api.Remote); !ok || r.Address != "lab:8443" {
		t.Errorf("--context: %#v", pick("--context", "lab"))
	}
	if _, ok := pick("--data-dir", "/var/lib/oikade").(api.Local); !ok {
		t.Error("--data-dir should mean this machine")
	}
	if _, ok := pick("--context", "nowhere").(failing); !ok {
		t.Error("an unknown context should fail")
	}
	t.Setenv(tokenEnv, "oik_ci")
	if r, _ := pick("--context", "lab").(api.Remote); r.Token != "oik_ci" {
		t.Errorf("a context with no token should take %s: %#v", tokenEnv, r)
	}
	if r, _ := pick().(api.Remote); r.Token != "oik_pi" {
		t.Errorf("%s went to a host with its own token: %#v", tokenEnv, r)
	}
}
