package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/cli"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
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

// hintOf is a cli.Error's hint.
func hintOf(err error) string {
	if e, ok := errors.AsType[*cli.Error](err); ok {
		return e.Hint
	}
	return ""
}

func usesNothing(context.Context, string, string) (wire.Manifest, error) { return wire.Manifest{}, nil }

func noEnvironment(string) (string, bool) { return "", false }

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
		manifest: usesNothing,
		lookup:   noEnvironment,
	}
	runErr := d.run(t.Context(), host, t.TempDir(), binary, "", false)
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
	want := "Checking your code\nBuilding for linux/arm64\nUploading 3.0 MB\nWaiting for the host to check and settle\n"
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
	// Five beats later, there's still nothing more.
	time.Sleep(50 * time.Millisecond)
	if b.String() != got {
		t.Error("the heartbeat went on after stop")
	}
}

func TestDeployFailsWithoutAHost(t *testing.T) {
	var b bytes.Buffer
	d := deployer{stdout: &b, progress: &progress{w: &b}, manifest: usesNothing, lookup: noEnvironment}
	err := d.run(t.Context(), fakeHost{addr: "127.0.0.1:1"}, t.TempDir(), binaryOf(t, 10), "", false)
	if err == nil || !strings.HasSuffix(b.String(), "Uploading 10 B\n") {
		t.Errorf("err %v, output %q", err, b.String())
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 999: "999 B", 1500: "1.5 kB", 840_000: "840 kB", 9_960_000: "10 MB", 11_400_000: "11 MB",
	} {
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

func TestADumbTerminalGetsNoRedraws(t *testing.T) {
	for _, c := range []struct {
		terminal bool
		term     string
		want     bool
	}{
		{true, "xterm-256color", true},
		{true, "", true},
		{true, "dumb", false},
		{false, "xterm-256color", false},
	} {
		if got := redraws(c.terminal, c.term); got != c.want {
			t.Errorf("redraws(%v, %q) = %v, want %v", c.terminal, c.term, got, c.want)
		}
	}
}

func TestADeploySaysWhatItsDeclarationsChangedAndWarns(t *testing.T) {
	var out bytes.Buffer
	printDeviceChanges(&out, api.Response{
		DeviceChanges: []string{"Moved desk-plug to loft"},
		Warning:       "study has no devices now that case/fan/on/stuffy read, so they keep their state until one is back",
	})
	want := "Moved desk-plug to loft\nWarning: study has no devices now that case/fan/on/stuffy read"
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("got %q, want it to start %q", out.String(), want)
	}
	out.Reset()
	printDeviceChanges(&out, api.Response{})
	if out.Len() != 0 {
		t.Errorf("no changes printed %q", out.String())
	}
}

// recordingHost is a host that answers info and a deploy with answer, and keeps the values each
// deploy sent.
type recordingHost struct {
	fakeHost
	mu   sync.Mutex
	sent []map[string]string
}

func startRecordingHost(t *testing.T, answer api.Response) *recordingHost {
	t.Helper()
	h := &recordingHost{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_ = json.NewEncoder(w).Encode(api.Response{GOOS: "linux", GOARCH: "arm64"})
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
			return
		}
		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		if err != nil || part.FormName() != api.DeployVarsPart {
			t.Errorf("the first part: %v", err)
			return
		}
		var vars map[string]string
		if err := json.NewDecoder(part).Decode(&vars); err != nil {
			t.Error(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		h.mu.Lock()
		h.sent = append(h.sent, vars)
		h.mu.Unlock()
		if answer.Error != "" {
			w.Header().Set("Content-Type", api.ContentType)
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	h.addr = srv.Listener.Addr().String()
	return h
}

func (h *recordingHost) deploys() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sent)
}

// home is a module with a go.mod and, unless it's empty, a .env.
func homeWithValues(t *testing.T, env string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// deployValues deploys a module whose code uses used, with environment as the environment, to host.
func deployValues(t *testing.T, host *recordingHost, module, envFile string, used []string,
	environment map[string]string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	d := deployer{
		stdout:   &out,
		progress: &progress{w: &errOut},
		check:    func(string) error { return nil },
		build: func(_ context.Context, _, out string, _ api.Response) error {
			return os.WriteFile(out, []byte("a binary"), 0o600)
		},
		manifest: func(context.Context, string, string) (wire.Manifest, error) {
			return wire.Manifest{Vars: used}, nil
		},
		lookup: func(name string) (string, bool) { v, ok := environment[name]; return v, ok },
	}
	err := d.run(t.Context(), host, module, "", envFile, false)
	if strings.Contains(out.String()+errOut.String(), "hunter2") {
		t.Errorf("a value was printed: %s%s", out.String(), errOut.String())
	}
	return out.String(), err
}

func TestADeploySendsTheFilesValuesAndTheEnvironmentWins(t *testing.T) {
	host := startRecordingHost(t, api.Response{Version: "v2"})
	module := homeWithValues(t, "TOSHIBA_USER=someone\nTOSHIBA_PASSWORD=old\nNOT_USED=kept\n")
	out, err := deployValues(t, host, module, "", []string{"TOSHIBA_PASSWORD", "TAHOMA_PIN"},
		map[string]string{"TOSHIBA_PASSWORD": "hunter2", "TAHOMA_PIN": "1234", "UNRELATED": "not sent"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"TOSHIBA_USER": "someone", "TOSHIBA_PASSWORD": "hunter2", "NOT_USED": "kept", "TAHOMA_PIN": "1234",
	}
	if got := host.deploys(); len(got) != 1 || !maps.Equal(got[0], want) {
		t.Errorf("sent %q", got)
	}
	line := ", with 4 values from " + filepath.Join(module, ".env") + " and the environment\n"
	if !strings.HasSuffix(out, line) {
		t.Errorf("got %q, want it to end %q", out, line)
	}
}

func TestADeploySaysWhereItsValuesCameFrom(t *testing.T) {
	for name, c := range map[string]struct {
		env, want   string
		used        []string
		environment map[string]string
	}{
		"none":              {"", "Deployed version v2\n", nil, nil},
		"one from the file": {"A=1\n", "Deployed version v2, with 1 value from .env\n", nil, nil},
		"all from the file": {"A=1\nB=2\n", "Deployed version v2, with 2 values from .env\n", nil, nil},
		"from the environment": {
			"", "Deployed version v2, with 1 value from the environment\n", []string{"A"}, map[string]string{"A": "1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			module := homeWithValues(t, c.env)
			t.Chdir(module)
			out, err := deployValues(t, startRecordingHost(t, api.Response{Version: "v2"}), ".", "", c.used, c.environment)
			if err != nil || !strings.HasSuffix(out, c.want) {
				t.Errorf("got %q, %v, want it to end %q", out, err, c.want)
			}
		})
	}
}

func TestADeployMissingAValueStopsBeforeItsUpload(t *testing.T) {
	host := startRecordingHost(t, api.Response{Version: "v2"})
	module := homeWithValues(t, "TOSHIBA_USER=someone\n")
	t.Chdir(module)
	_, err := deployValues(t, host, ".", "", []string{"TOSHIBA_USER", "TOSHIBA_PASSWORD"}, nil)
	if err == nil || err.Error() != "TOSHIBA_PASSWORD isn't set" ||
		hintOf(err) != "add it to .env, or set it in the environment" {
		t.Errorf("got %v, hint %q", err, hintOf(err))
	}
	_, err = deployValues(t, host, ".", "", []string{"TAHOMA_PIN", "TOSHIBA_PASSWORD"}, nil)
	if err == nil || err.Error() != "TAHOMA_PIN and TOSHIBA_PASSWORD aren't set" ||
		hintOf(err) != "add them to .env, or set them in the environment" {
		t.Errorf("got %v, hint %q", err, hintOf(err))
	}
	if got := host.deploys(); len(got) != 0 {
		t.Errorf("uploaded %d deploys", len(got))
	}
}

func TestADeployReadsTheEnvFileItsGiven(t *testing.T) {
	host := startRecordingHost(t, api.Response{Version: "v2"})
	module := homeWithValues(t, "A=from .env\n")
	other := filepath.Join(t.TempDir(), "test-host.env")
	if err := os.WriteFile(other, []byte("export A='from the other file'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deployValues(t, host, module, other, []string{"A"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := host.deploys(); len(got) != 1 || got[0]["A"] != "from the other file" {
		t.Errorf("sent %q", got)
	}
	_, err := deployValues(t, host, module, "missing.env", nil, nil)
	if err == nil || err.Error() != "there's no missing.env" {
		t.Errorf("got %v", err)
	}
}

func TestADeploySaysWhereItsEnvFileIsWrong(t *testing.T) {
	module := homeWithValues(t, "A=1\ntoshiba_password=hunter2\n")
	t.Chdir(module)
	_, err := deployValues(t, startRecordingHost(t, api.Response{Version: "v2"}), ".", "", nil, nil)
	if err == nil || !strings.HasPrefix(err.Error(), `.env:2: "toshiba_password" isn't a variable name`) ||
		!strings.Contains(err.Error(), `such as "TOSHIBA_PASSWORD"`) {
		t.Errorf("got %v", err)
	}
}

func TestADeployOfOnlyNewValuesSaysWhichChanged(t *testing.T) {
	host := startRecordingHost(t, api.Response{Version: "v2", Unchanged: true, VarsChanged: []string{"A", "B"}})
	out, err := deployValues(t, host, homeWithValues(t, "A=1\nB=2\n"), "", nil, nil)
	if err != nil || out != "Deployed version v2 again, with A and B changed\n" {
		t.Errorf("got %q, %v", out, err)
	}
	host = startRecordingHost(t, api.Response{Version: "v2", Unchanged: true})
	out, err = deployValues(t, host, homeWithValues(t, "A=1\n"), "", nil, nil)
	if err != nil || out != "Already running version v2\n" {
		t.Errorf("got %q, %v", out, err)
	}
}

func TestAVersionTheHostRefusesForAMissingValueSaysWhereToAddIt(t *testing.T) {
	host := startRecordingHost(t, api.Response{
		Error: "the version uses A, which isn't among the deploy's values", MissingVars: []string{"A"},
	})
	module := homeWithValues(t, "")
	t.Chdir(module)
	_, err := deployValues(t, host, ".", "", nil, nil)
	if err == nil || err.Error() != "the version uses A, which isn't among the deploy's values" ||
		hintOf(err) != "add it to .env" {
		t.Errorf("got %v, hint %q", err, hintOf(err))
	}
}
