package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

func uploadServer(t *testing.T, ctx context.Context) (*apiServer, string) {
	t.Helper()
	dir := t.TempDir()
	if err := clearUploads(dir); err != nil {
		t.Fatal(err)
	}
	return &apiServer{log: slog.New(slog.DiscardHandler), ctx: ctx, dataDir: dir}, dir
}

func TestAnUploadStopsWhenItsCallerIsCutOff(t *testing.T) {
	s, dir := uploadServer(t, context.Background())
	failed := make(chan error, 1)
	cutOff, cut := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, err := s.spool(cutOff, w, r)
		if err == nil {
			_ = os.Remove(path)
		}
		failed <- err
	}))
	defer server.Close()

	body, sending := io.Pipe()
	go func() {
		_, _ = sending.Write([]byte("partial"))
		// Then nothing, as a slow link or a revoked client would.
	}()
	go func() {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/deploy", body)
		req.ContentLength = 1 << 20
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cut()
	select {
	case err := <-failed:
		if err == nil {
			t.Error("a cut-off upload was taken")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upload kept waiting after its caller was cut off")
	}
	_ = sending.Close()
	if left, _ := os.ReadDir(filepath.Join(dir, uploadsDir)); len(left) != 0 {
		t.Errorf("left %v behind", left)
	}
}

func TestADeployOfSomethingElseIsRefused(t *testing.T) {
	s, _ := uploadServer(t, context.Background())
	server := httptest.NewServer(http.HandlerFunc(s.deployUpload))
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/deploy", api.ContentType, strings.NewReader(`{"binary":"/tmp/home"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a JSON deploy: %d", resp.StatusCode)
	}

	// A real request that says it's over the limit, and starts sending: the refusal comes before
	// any of it is read, so a client finds out at once rather than after a gigabyte.
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "POST /v1/deploy HTTP/1.1\r\nHost: x\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		api.BinaryContentType, maxUpload+1); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a binary over the limit: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a binary over the limit: %d", resp.StatusCode)
	}
}

func TestAnUploadIsMovedNotCopied(t *testing.T) {
	dir := t.TempDir()
	if err := clearUploads(dir); err != nil {
		t.Fatal(err)
	}
	v := &versions{dir: filepath.Join(dir, versionsDir), uploads: filepath.Join(dir, uploadsDir)}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "home")
	upload := filepath.Join(v.uploads, "deploy-1")
	for _, p := range []string{other, upload} {
		if err := os.WriteFile(p, []byte("a binary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copied, err := v.add(other)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := v.take(upload)
	if err != nil {
		t.Fatal(err)
	}
	if moved != copied {
		t.Errorf("the same binary got IDs %s and %s", copied, moved)
	}
	if _, err := os.Stat(upload); !os.IsNotExist(err) {
		t.Errorf("the upload is still there: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a binary from elsewhere went: %v", err)
	}
}
