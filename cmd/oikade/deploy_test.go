package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// fakeHost is a host that answers info and takes any deploy, through a Transport.
type fakeHost struct{ addr string }

func (h fakeHost) Dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", h.addr)
}

func startFakeHost(t *testing.T) fakeHost {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := api.Response{GOOS: "linux", GOARCH: "arm64"}
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			resp = api.Response{Version: "v2"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return fakeHost{addr: srv.Listener.Addr().String()}
}

// binaryOf is a file of n bytes.
func binaryOf(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// deployTo runs a deploy of the module, or binary, with stderr to a pipe, and returns what it wrote.
func deployTo(t *testing.T, host fakeHost, binary string, live bool) (stdout, stderr string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		read <- b
	}()
	var out bytes.Buffer
	d := deployer{
		stdout:   &out,
		progress: &progress{w: w, live: live},
		check:    func(string) error { return nil },
		build: func(_ context.Context, _, out string, _ api.Response) error {
			return os.WriteFile(out, make([]byte, 3_000_000), 0o600)
		},
	}
	runErr := d.run(context.Background(), host, t.TempDir(), binary, false)
	_ = w.Close()
	errOut := <-read
	if runErr != nil {
		t.Fatalf("deploy: %v\n%s", runErr, errOut)
	}
	return out.String(), string(errOut)
}

func TestDeploySaysEachPhaseBeforeItStarts(t *testing.T) {
	host := startFakeHost(t)
	stdout, stderr := deployTo(t, host, "", false)
	want := "Checking flows\nBuilding for linux/arm64\nUploading 3.0 MB\nWaiting for the host to check and settle\n"
	if stderr != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", stderr, want)
	}
	if stdout != "Deployed version v2\n" {
		t.Errorf("stdout: %q", stdout)
	}
}

func TestDeployOfABinarySkipsTheSourcePhases(t *testing.T) {
	host := startFakeHost(t)
	_, stderr := deployTo(t, host, binaryOf(t, 840_000), false)
	if want := "Uploading 840 kB\nWaiting for the host to check and settle\n"; stderr != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", stderr, want)
	}
}

func TestDeployToAPipeRedrawsNothing(t *testing.T) {
	host := startFakeHost(t)
	_, stderr := deployTo(t, host, binaryOf(t, 5_000_000), false)
	if strings.ContainsAny(stderr, "\r\x1b") || strings.Contains(stderr, "sent") {
		t.Errorf("stderr to a pipe has redraws: %q", stderr)
	}
}

func TestDeployOnATerminalShowsTheBytesSent(t *testing.T) {
	host := startFakeHost(t)
	_, stderr := deployTo(t, host, binaryOf(t, 5_000_000), true)
	if !strings.HasPrefix(stderr, "Uploading 5.0 MB\rUploading 5.0 MB: ") {
		t.Errorf("no live counter: %q", stderr)
	}
	want := "\rUploading 5.0 MB: 5.0 MB sent\x1b[K\nWaiting for the host to check and settle\n"
	if !strings.HasSuffix(stderr, want) {
		t.Errorf("stderr:\n%q\nwant it to end:\n%q", stderr, want)
	}
}

func TestProgressEndsALineAnUploadLeftOpen(t *testing.T) {
	var b bytes.Buffer
	p := &progress{w: &b, live: true}
	p.startUpload(1000)
	p.upload(400, 1000)
	p.end()
	p.end()
	if want := "Uploading 1.0 kB\rUploading 1.0 kB: 400 B sent\x1b[K\n"; b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestHeartbeatSaysABuildIsStillGoing(t *testing.T) {
	var b syncBuffer
	stop := heartbeat(&progress{w: &b}, "building", 10*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), "Still building, ") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	got := b.String()
	if !strings.HasPrefix(got, "Still building, ") || !strings.HasSuffix(got, " so far\n") {
		t.Errorf("got %q", got)
	}
	if b.String() != got {
		t.Error("the heartbeat went on after stop")
	}
}

func TestDeployFailsWithoutAHost(t *testing.T) {
	var b bytes.Buffer
	d := deployer{stdout: &b, progress: &progress{w: &b}}
	err := d.run(context.Background(), fakeHost{addr: "127.0.0.1:1"}, "", binaryOf(t, 10), false)
	if err == nil || !strings.HasSuffix(b.String(), "Uploading 10 B\n") {
		t.Errorf("err %v, output %q", err, b.String())
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1500: "1.5 kB", 840_000: "840 kB", 11_400_000: "11 MB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}

// syncBuffer is a bytes.Buffer for a writer in another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
