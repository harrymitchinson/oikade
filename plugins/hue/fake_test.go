package hue

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	bridgeOne = "C42996FFFEC84D62"
	bridgeTwo = "001788FFFE0A0B0C"
	appKey    = "oikade-test-key"
)

// authority is a test CA, which signs bridges' certificates as Philips Hue's root-bridge does: the
// bridge's ID as the CN, and no SANs.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAuthority(t testing.TB) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{Country: []string{"NL"}, Organization: []string{"Philips Hue"}, CommonName: "root-bridge"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &authority{cert: cert, key: key, pool: pool}
}

// issue is a bridge's certificate, for its ID, valid from notBefore to notAfter.
func (a *authority) issue(t testing.TB, id string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{Country: []string{"NL"}, Organization: []string{"Philips Hue"}, CommonName: id, OrganizationalUnit: []string{"BSB003"}},
		NotBefore:    notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeBridge is a Hue bridge: its resources, and its event stream, over HTTPS and HTTP/2 with a
// certificate from a test authority.
type fakeBridge struct {
	t      *testing.T
	server *httptest.Server
	ca     *authority

	mu        sync.Mutex
	resources []json.RawMessage
	// refuse is the status every request is answered with instead, 0 for none.
	refuse int
	// reads and streams count the reads of every resource and the streams opened.
	reads, streams int
	// events go to the stream that's open, and end ends it.
	events chan string
	end    chan struct{}
	// open is closed once a stream is open, and replaced when it ends.
	open chan struct{}
	seq  int
	// puts are the writes, putError what the bridge answers one with instead, "" for none, and
	// hang whether it answers none.
	puts     []put
	putError string
	hang     bool
}

// put is one write the bridge took.
type put struct {
	light, body string
	at          time.Time
}

func newFakeBridge(t *testing.T, id string) *fakeBridge {
	t.Helper()
	raw, err := os.ReadFile("testdata/resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	f := &fakeBridge{t: t, ca: newAuthority(t), resources: a.Data, events: make(chan string, 64), end: make(chan struct{}), open: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /clip/v2/resource", f.read)
	mux.HandleFunc("GET /eventstream/clip/v2", f.stream)
	mux.HandleFunc("PUT /clip/v2/resource/light/{id}", f.put)
	f.server = httptest.NewUnstartedServer(mux)
	f.server.EnableHTTP2 = true
	f.server.TLS = &tls.Config{Certificates: []tls.Certificate{f.ca.issue(t, id, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

// address is where the bridge listens.
func (f *fakeBridge) address() string { return f.server.Listener.Addr().String() }

// allowed answers a request the bridge refuses, and reports whether it may go on.
func (f *fakeBridge) allowed(w http.ResponseWriter, r *http.Request) bool {
	if r.ProtoMajor != 2 {
		f.t.Errorf("%s over %s, want HTTP/2", r.URL.Path, r.Proto)
	}
	f.mu.Lock()
	refuse := f.refuse
	f.mu.Unlock()
	if refuse == 0 && r.Header.Get("hue-application-key") != appKey {
		refuse = http.StatusForbidden
	}
	if refuse != 0 {
		// The bridge answers a refused key with a page, not JSON.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(refuse)
		_, _ = fmt.Fprint(w, "<html><body>Oops, there appears to be no lighting here</body></html>")
		return false
	}
	return true
}

func (f *fakeBridge) read(w http.ResponseWriter, r *http.Request) {
	if !f.allowed(w, r) {
		return
	}
	f.mu.Lock()
	f.reads++
	body, err := json.Marshal(answer{Data: f.resources})
	f.mu.Unlock()
	if err != nil {
		f.t.Error(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (f *fakeBridge) stream(w http.ResponseWriter, r *http.Request) {
	if !f.allowed(w, r) {
		return
	}
	if r.Header.Get("Accept") != "text/event-stream" {
		f.t.Errorf("opened the stream accepting %q", r.Header.Get("Accept"))
	}
	f.mu.Lock()
	f.streams++
	open, end := f.open, f.end
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, ": hi\n\n")
	flusher := w.(http.Flusher)
	flusher.Flush()
	close(open)
	for {
		select {
		case e := <-f.events:
			_, _ = fmt.Fprint(w, e)
			flusher.Flush()
		case <-end:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// put takes a write and, as the bridge does, sends an event with what it changed.
func (f *fakeBridge) put(w http.ResponseWriter, r *http.Request) {
	if !f.allowed(w, r) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Errorf("wrote with Content-Type %q", ct)
	}
	id := r.PathValue("id")
	f.mu.Lock()
	f.puts = append(f.puts, put{light: id, body: string(body), at: time.Now()})
	refuse, hang := f.putError, f.hang
	f.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if refuse != "" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"errors":[{"description":%q}],"data":[]}`, refuse)
		return
	}
	_, _ = fmt.Fprintf(w, `{"errors":[],"data":[{"rid":%q,"rtype":"light"}]}`, id)
	var change map[string]any
	if err := json.Unmarshal(body, &change); err != nil {
		f.t.Errorf("wrote %s, which isn't JSON", body)
		return
	}
	change["id"], change["type"] = id, "light"
	event, _ := json.Marshal(change)
	// Not send, which can fail the test: this isn't the test's goroutine.
	select {
	case f.events <- f.event("update", string(event)):
	default:
		f.t.Error("the stream's events are full")
	}
}

func (f *fakeBridge) written() []put {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.puts)
}

func (f *fakeBridge) hangWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = true
}

func (f *fakeBridge) refuseWrites(description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putError = description
}

// waitOpen waits for a stream to be open.
func (f *fakeBridge) waitOpen() {
	f.t.Helper()
	f.mu.Lock()
	open := f.open
	f.mu.Unlock()
	select {
	case <-open:
	case <-time.After(waitLimit):
		f.t.Fatal("no stream opened")
	}
}

// send sends one event, of type kind, of the resources in data, to the open stream.
func (f *fakeBridge) send(kind string, data ...string) {
	f.t.Helper()
	f.waitOpen()
	f.events <- f.event(kind, data...)
}

// event is one event in the stream, of type kind, of the resources in data.
func (f *fakeBridge) event(kind string, data ...string) string {
	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.mu.Unlock()
	items := make([]json.RawMessage, 0, len(data))
	for _, d := range data {
		items = append(items, json.RawMessage(d))
	}
	payload, err := json.Marshal([]map[string]any{{
		"creationtime": "2026-11-24T10:07:54Z", "id": fmt.Sprintf("caeb36fd-0000-0000-0000-%012d", seq), "type": kind, "data": items,
	}})
	if err != nil {
		f.t.Error(err)
	}
	return fmt.Sprintf("id: 1795514874:%d\ndata: %s\n\n", seq, payload)
}

// endStream ends the open stream, as the bridge might.
func (f *fakeBridge) endStream() {
	f.t.Helper()
	f.waitOpen()
	f.mu.Lock()
	close(f.end)
	f.end, f.open = make(chan struct{}), make(chan struct{})
	f.mu.Unlock()
}

// add adds a resource, which the next read has.
func (f *fakeBridge) add(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources = append(f.resources, json.RawMessage(raw))
}

func (f *fakeBridge) setRefuse(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = status
}

func (f *fakeBridge) counts() (reads, streams int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.streams
}
