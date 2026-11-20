package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	s, dir := uploadServer(t, t.Context())
	failed := make(chan error, 1)
	cutOff, cut := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, path, err := s.spool(cutOff, w, r, "b")
		if err == nil {
			_ = os.Remove(path)
		}
		failed <- err
	}))
	defer server.Close()

	body, sending := io.Pipe()
	go func() {
		_, _ = sending.Write([]byte("--b\r\nContent-Disposition: form-data; name=\"vars\"\r\n\r\n{}\r\n" +
			"--b\r\nContent-Disposition: form-data; name=\"binary\"\r\n\r\npartial"))
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
	s, _ := uploadServer(t, t.Context())
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
		api.DeployContentType+"; boundary=b", maxUpload+1); err != nil {
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

// tcpHost reaches a test's HTTP server, as a Transport.
type tcpHost string

func (h tcpHost) Dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", string(h))
}

// received is what the host made of a deploy's body.
type received struct {
	vars   map[string]string
	binary []byte
	err    error
}

func receiving(t *testing.T) (*httptest.Server, <-chan received) {
	t.Helper()
	s, _ := uploadServer(t, t.Context())
	got := make(chan received, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		boundary, ok := deployBoundary(r.Header.Get("Content-Type"))
		if !ok {
			got <- received{err: fmt.Errorf("content type %q", r.Header.Get("Content-Type"))}
			return
		}
		vars, path, err := s.spool(r.Context(), w, r, boundary)
		var binary []byte
		if err == nil {
			binary, err = os.ReadFile(path)
			_ = os.Remove(path)
		}
		got <- received{vars, binary, err}
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(server.Close)
	return server, got
}

func TestADeployCarriesItsValuesBeforeItsBinary(t *testing.T) {
	server, got := receiving(t)
	path := filepath.Join(t.TempDir(), "home")
	binary := []byte("a binary\r\n--with a line like a boundary\r\n")
	if err := os.WriteFile(path, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TOSHIBA_USER": "someone", "TOSHIBA_PASSWORD": "pa\"ss\nword", "EMPTY": ""}
	var sent, total int64
	if _, err := api.Deploy(t.Context(), tcpHost(server.Listener.Addr().String()), path, vars, false,
		func(s, n int64) { sent, total = s, n }); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !maps.Equal(r.vars, vars) || string(r.binary) != string(binary) {
		t.Errorf("got %q and %q", r.vars, r.binary)
	}
	// The progress counts the binary, not its values.
	if sent != int64(len(binary)) || total != sent {
		t.Errorf("progress said %d of %d", sent, total)
	}
}

func TestADeployWithNoValuesSendsAnEmptySet(t *testing.T) {
	server, got := receiving(t)
	path := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Deploy(t.Context(), tcpHost(server.Listener.Addr().String()), path, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.err != nil || r.vars == nil || len(r.vars) != 0 || len(r.binary) != 0 {
		t.Errorf("got %+v", r)
	}
}

func TestADeploysBodyIsRefusedUnlessItsWhole(t *testing.T) {
	const part = "--b\r\nContent-Disposition: form-data; name=%q\r\n\r\n%s\r\n"
	vars := fmt.Sprintf(part, "vars", `{"A":"hunter2"}`)
	binary := fmt.Sprintf(part, "binary", "ELF")
	for name, c := range map[string]struct{ body, want string }{
		"no binary":           {vars + "--b--\r\n", "the deploy has no binary"},
		"binary first":        {binary + vars + "--b--\r\n", `the deploy has "binary" where its vars should be`},
		"cut off":             {vars + "--b\r\nContent-Disposition: form-data; name=\"binary\"\r\n\r\nEL", "the upload was cut short"},
		"no closing boundary": {vars + binary, "the upload was cut short"},
		"a part after":        {vars + binary + fmt.Sprintf(part, "more", "") + "--b--\r\n", "the deploy has a part after the binary"},
		"values not strings":  {fmt.Sprintf(part, "vars", `{"A":1}`) + binary + "--b--\r\n", "the values aren't a JSON object of strings"},
		"values not JSON":     {fmt.Sprintf(part, "vars", `{"A":"hunter2`) + binary + "--b--\r\n", "the values aren't a JSON object of strings"},
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := uploadServer(t, t.Context())
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/deploy", strings.NewReader(c.body))
			_, _, err := s.spool(t.Context(), w, r, "b")
			if err == nil || err.Error() != c.want {
				t.Errorf("got %v, want %s", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error quotes a value: %v", err)
			}
			if left, _ := os.ReadDir(filepath.Join(dir, uploadsDir)); len(left) != 0 {
				t.Errorf("left %v behind", left)
			}
		})
	}
}
