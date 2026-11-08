package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// oikade tail says so when the host ends its stream, and fails, so a script sees it stopped.
func TestTailSaysTheHostStopped(t *testing.T) {
	// Not t.TempDir: a Unix socket's path has to fit in about 100 bytes.
	dir, err := os.MkdirTemp("", "oik")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, api.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	host := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.StreamContentType)
		_, _ = w.Write([]byte("{}\n"))
	})}
	go func() { _ = host.Serve(l) }()
	t.Cleanup(func() { _ = host.Close() })

	err = runTail(t.Context(), []string{"--data-dir", dir})
	if !errors.Is(err, api.ErrHostStopped) || err.Error() != "the host stopped" {
		t.Errorf("oikade tail ended with %v", err)
	}
}
