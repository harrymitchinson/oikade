package main

import "testing"

func TestPublishedVersionIsOneGoCanFetch(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "v0.0.0-2026-abc+dirty"
	if got := publishedVersion(); got != "main" {
		t.Errorf("a local build: %s", got)
	}
	version = "v0.2.0"
	if got := publishedVersion(); got != "v0.2.0" {
		t.Errorf("a release: %s", got)
	}
}
