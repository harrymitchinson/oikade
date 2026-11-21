package plugin

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"oikade.dev/contract/v1"
)

func TestAPluginIsStuckOnlyOnceASetRunsTooLong(t *testing.T) {
	life, stop := context.WithCancel(t.Context())
	defer stop()
	p := newPlugin(contract.NewRoot("relays"), slog.Default(), nil, time.Second, life, stop)
	if p.stuck() {
		t.Fatal("stuck with nothing running")
	}
	p.running["relay"] = time.Now().Add(-time.Hour)
	if p.stuck() {
		t.Fatal("stuck with no limit set")
	}
	p.setTimeout = time.Minute
	if !p.stuck() {
		t.Fatal("not stuck an hour into a Set")
	}
	p.running["relay"] = time.Now()
	if p.stuck() {
		t.Fatal("stuck on a Set that has just begun")
	}
}
