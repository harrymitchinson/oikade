package host

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const servingAPI = "Serving API over TLS"

// logged is a field of the last log line with this message, anywhere in the log.
func (r *rig) logged(msg, field string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := ""
	for _, l := range r.lines {
		if l.is(msg) {
			value, _ = l[field].(string)
		}
	}
	return value
}

// network is where a rig's host serves its API over TLS, and the pin it serves it with. The host says
// so before it's attached.
func (r *rig) network() (address, pin string) {
	r.t.Helper()
	address, pin = r.logged(servingAPI, "address"), r.logged(servingAPI, "pin")
	if address == "" {
		r.t.Fatalf("the host isn't serving its API over TLS. The log:\n%s", r.dump())
	}
	return address, pin
}

// addClient makes a client with a token, as CI's would be.
func (r *rig) addClient(name string) (token, pin string) {
	r.t.Helper()
	var added struct{ Token, Pin string }
	out := r.stdout(nil, "client", "add", name, "--token", "--json")
	if err := json.Unmarshal(out, &added); err != nil {
		r.t.Fatalf("reading client add: %v: %s", err, out)
	}
	return added.Token, added.Pin
}

// pinned is a client that trusts only the host with this pin.
func pinned(pin string) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: api.PinnedTLS(pin)}}
}

// call makes a call over TLS, and returns its status and answer.
func call(t *testing.T, c *http.Client, address, token, method, path, body string) (int, api.Response) {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+address+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer api.Response
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, raw)
	}
	return resp.StatusCode, answer
}

func TestNetworkAPINeedsAValidToken(t *testing.T) {
	t.Parallel()
	r := start(t, "--api-addr", "127.0.0.1:0")
	address, pin := r.network()
	token, added := r.addClient("ci")
	if added != pin {
		t.Fatalf("client add says the pin is %s, and the host %s", added, pin)
	}
	c := pinned(pin)

	if status, _ := call(t, c, address, "", "GET", "/v1/status", ""); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}
	if status, _ := call(t, c, address, "oik_nothing", "GET", "/v1/status", ""); status != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d", status)
	}
	status, answer := call(t, c, address, token, "GET", "/v1/status", "")
	if status != http.StatusOK || answer.Status == nil {
		t.Fatalf("a good token: %d %+v", status, answer)
	}
	r.waitFor(func(l line) bool { return l.is("Received API request", "type", api.TypeStatus, "client", "ci") })

	// A stolen token can't make more of them.
	status, answer = call(t, c, address, token, "POST", "/v1/clients/add", `{"client":"more"}`)
	if status == http.StatusOK || !strings.Contains(answer.Error, "only the host's machine") {
		t.Errorf("client add over TLS: %d %+v", status, answer)
	}
	// But a lost laptop can be cut off from anywhere, itself included.
	status, answer = call(t, c, address, token, "POST", "/v1/clients/revoke", `{"client":"ci"}`)
	if status != http.StatusOK {
		t.Fatalf("revoking: %d %+v", status, answer)
	}
	if status, _ := call(t, c, address, token, "GET", "/v1/status", ""); status != http.StatusUnauthorized {
		t.Errorf("a revoked token: %d", status)
	}
	if got := r.metric("oikade_api_auth_failures_total"); got != "3" {
		t.Errorf("auth failures: %q, want 3", got)
	}

	// A client that pins something else never sends its token.
	if _, err := pinned("sha256:00").Get("https://" + address + "/v1/status"); err == nil {
		t.Error("a wrong pin connected")
	}
}

func TestRevokingEndsATailAndAStrangerCantHoldAConnection(t *testing.T) {
	t.Parallel()
	r := start(t, "--api-addr", "127.0.0.1:0")
	address, pin := r.network()
	token, _ := r.addClient("laptop")
	c := pinned(pin)

	req, err := http.NewRequest("GET", "https://"+address+"/v1/tail", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	r.stdout(nil, "client", "revoke", "laptop")
	select {
	case <-ended:
	case <-time.After(waitLimit):
		t.Error("a revoked client's tail kept streaming")
	}

	// A body promised and never sent: the refusal comes at once, and the connection goes.
	// It's the test's own host, and the call never gets as far as anything a pin protects.
	conn, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "POST /v1/deploy HTTP/1.1\r\nHost: x\r\nContent-Length: 200000\r\n\r\n{")
	_ = conn.SetReadDeadline(time.Now().Add(waitLimit))
	answer, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = answer.Body.Close()
	if answer.StatusCode != http.StatusUnauthorized || !answer.Close {
		t.Errorf("a stranger's call: %d, close %v", answer.StatusCode, answer.Close)
	}

	// A login needs no token, so it can't take a big body either.
	big := `{"code":"` + strings.Repeat("x", 8<<10) + `"}`
	if status, _ := call(t, c, address, "", "POST", "/v1/login", big); status != http.StatusBadRequest {
		t.Errorf("a login with an 8 KiB body: %d", status)
	}
}

func TestNetworkAPIKeepsItsIdentity(t *testing.T) {
	t.Parallel()
	r := start(t, "--api-addr", "127.0.0.1:0")
	_, pin := r.network()
	token, _ := r.addClient("laptop")
	r.stop()

	restored := &rig{t: t, dir: copyDataDir(t, r.dir), grew: make(chan struct{})}
	restored.launch("--api-addr", "127.0.0.1:0")
	restored.waitFor(func(l line) bool { return l.is(attached) })
	address, again := restored.network()
	if again != pin {
		t.Errorf("the pin went from %s to %s", pin, again)
	}
	if status, answer := call(t, pinned(pin), address, token, "GET", "/v1/status", ""); status != http.StatusOK {
		t.Errorf("the token after a restore: %d %+v", status, answer)
	}
}

func TestATakenAPIPortLeavesTheHostRunning(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	r := start(t, "--api-addr", taken.Addr().String())
	if r.count("Failed to open the API listener") == 0 {
		t.Errorf("the host didn't say why it has no network API. The log:\n%s", r.dump())
	}
	r.hostRunning()
	if st := r.status(); st.Devices == nil {
		t.Errorf("status over the socket: %+v", st)
	}
	if _, err := r.oikade(nil, "client", "add", "phone"); err == nil {
		t.Error("issued a login code with no listener for it")
	}
	// A token must stay revocable while the port is taken, or it would work again once it's free.
	r.addClient("laptop")
	if stderr, err := r.oikade(nil, "client", "revoke", "laptop"); err != nil {
		t.Errorf("revoking with no listener: %v: %s", err, stderr)
	}
}

// metric is a host metric's value, with no labels, as /metrics shows it.
func (r *rig) metric(name string) string {
	r.t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", r.logged("Serving metrics and health", "address")))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	for row := range strings.SplitSeq(string(raw), "\n") {
		if value, ok := strings.CutPrefix(row, name+" "); ok {
			return value
		}
	}
	return ""
}
