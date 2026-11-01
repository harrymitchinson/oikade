package hap

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/tlv8"
	"github.com/go-chi/chi"
	"github.com/go-logr/logr"
)

// tlvMethod is the TLV type of a request's method.
const tlvMethod byte = 0x00

// tlvValue returns the value of the first item of type typ in b, joining its
// fragments. ok is false if there's none, or b is malformed.
func tlvValue(b []byte, typ byte) (value []byte, ok bool) {
	found := false
	for len(b) > 0 {
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			return nil, false
		}
		t, v := b[0], b[2:2+int(b[1])]
		b = b[2+int(b[1]):]
		switch {
		case t == typ:
			value = append(value, v...)
			found = true
		case found:
			return value, true
		}
	}
	return value, found
}

// The bodies controllers send, as in the HAP specification.
var (
	listRequest   = []byte{tlvState, 1, 1, tlvMethod, 1, hap.MethodListPairings}
	removeRequest = append([]byte{tlvState, 1, 1, tlvMethod, 1, hap.MethodDeletePairing, tlvIdentifier, 3}, "hub"...)
)

// savePairing stores p the way brutella/hap does.
func savePairing(t *testing.T, store hap.Store, p hap.Pairing) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(hex.EncodeToString([]byte(p.Name))+".pairing", b); err != nil {
		t.Fatal(err)
	}
}

func post(h http.Handler, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, pairingsPath, bytes.NewReader(body)))
	return rec
}

// The wrapper works around brutella/hap v0.0.35. The library's handler can't be
// driven past its session check from a test, so an upgrade fails here instead:
// check whether the new version decodes List Pairings, and delete the wrapper
// if it does.
func TestWorkaroundIsForThisHAPVersion(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info")
	}
	for _, m := range info.Deps {
		if m.Path == "github.com/brutella/hap" && m.Version != "v0.0.35" {
			t.Fatalf("brutella/hap is %s: check whether it decodes List Pairings, then update this test or remove pairings.go", m.Version)
		}
	}
}

// A copy of the struct the library decodes /pairings into fails on List
// Pairings, which is the bug.
func TestLibraryCannotDecodeListPairings(t *testing.T) {
	d := struct {
		Method     byte   `tlv8:"0"`
		Identifier string `tlv8:"1"`
		PublicKey  []byte `tlv8:"3,optional"`
		Permission byte   `tlv8:"11,optional"`
		State      byte   `tlv8:"6"`
	}{}
	if err := tlv8.UnmarshalReader(bytes.NewReader(listRequest), &d); err == nil {
		t.Fatal("brutella/hap now decodes List Pairings; check whether fixListPairings is still needed")
	}
}

func TestListPairingsAnswersFromTheStore(t *testing.T) {
	store := hap.NewMemStore()
	savePairing(t, store, hap.Pairing{Name: "phone", PublicKey: []byte{1, 2, 3}, Permission: 1})
	savePairing(t, store, hap.Pairing{Name: "hub", PublicKey: []byte{4, 5}, Permission: 0})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("List Pairings reached the library") })

	rec := post(listPairingsHandler(next, store, func(*http.Request) bool { return true }, logr.Discard()), listRequest)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != hap.HTTPContentTypePairingTLV8 {
		t.Errorf("Content-Type %q", ct)
	}
	want := []byte{tlvState, 1, stateM2}
	want = append(append(want, tlvIdentifier, 3), "hub"...)
	want = append(want, tlvPublicKey, 2, 4, 5, tlvPermission, 1, 0)
	want = append(want, tlvSeparator, 0)
	want = append(append(want, tlvIdentifier, 5), "phone"...)
	want = append(want, tlvPublicKey, 3, 1, 2, 3, tlvPermission, 1, 1)
	if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
		t.Errorf("body\n got % x\nwant % x", got, want)
	}
}

func TestListPairingsWithNoPairings(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	rec := post(listPairingsHandler(next, hap.NewMemStore(), func(*http.Request) bool { return true }, logr.Discard()), listRequest)
	if got, want := rec.Body.Bytes(), []byte{tlvState, 1, stateM2}; !bytes.Equal(got, want) {
		t.Errorf("body % x, want % x", got, want)
	}
}

func TestListPairingsNeedsAVerifiedController(t *testing.T) {
	store := hap.NewMemStore()
	savePairing(t, store, hap.Pairing{Name: "phone", PublicKey: []byte{1}, Permission: 1})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("List Pairings reached the library") })

	rec := post(listPairingsHandler(next, store, func(*http.Request) bool { return false }, logr.Discard()), listRequest)

	if bytes.Contains(rec.Body.Bytes(), []byte("phone")) {
		t.Fatalf("listed pairings to an unverified controller: %q", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("-70401")) {
		t.Errorf("body %q, want the insufficient-privileges status", rec.Body.String())
	}
}

func TestOtherPairingRequestsReachTheLibraryIntact(t *testing.T) {
	var got []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusTeapot)
	})
	for name, body := range map[string][]byte{"remove": removeRequest, "empty": {}, "malformed": {tlvMethod, 9}} {
		got = nil
		rec := post(listPairingsHandler(next, hap.NewMemStore(), func(*http.Request) bool { return true }, logr.Discard()), body)
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s: status %d, want the library's", name, rec.Code)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("%s: library got % x, want % x", name, got, body)
		}
	}
}

func TestAppendTLVFragmentsLongValues(t *testing.T) {
	long := bytes.Repeat([]byte{7}, 300)
	b := appendTLV(nil, tlvPublicKey, long)
	if len(b) != 2+255+2+45 || b[1] != 255 || b[257] != tlvPublicKey || b[258] != 45 {
		t.Fatalf("fragments % x", b[:4])
	}
	if v, ok := tlvValue(b, tlvPublicKey); !ok || !bytes.Equal(v, long) {
		t.Errorf("round trip lost the value")
	}
}

func TestFixListPairingsOnARealServer(t *testing.T) {
	store := hap.NewMemStore()
	bridge := accessory.NewBridge(accessory.Info{Name: "Test"})
	server, err := hap.NewServer(store, bridge.A)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixListPairings(server, store, logr.Discard()); err != nil {
		t.Fatal(err)
	}

	// Without a verified session, List Pairings is refused, and nothing leaks.
	savePairing(t, store, hap.Pairing{Name: "phone", PublicKey: []byte{1}, Permission: 1})
	rec := post(server.ServeMux().(http.Handler), listRequest)
	if bytes.Contains(rec.Body.Bytes(), []byte("phone")) || !bytes.Contains(rec.Body.Bytes(), []byte("-70401")) {
		t.Errorf("unverified: body %q", rec.Body.String())
	}
}

// With the route replaced, a verified controller's List Pairings gets the
// wrapper's answer through the server's own router, and the library still
// handles the rest.
func TestWrapPairingsReplacesTheRoute(t *testing.T) {
	store := hap.NewMemStore()
	savePairing(t, store, hap.Pairing{Name: "phone", PublicKey: []byte{1}, Permission: 1})
	server, err := hap.NewServer(store, accessory.NewBridge(accessory.Info{Name: "Test"}).A)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapPairings(server, store, func(*http.Request) bool { return true }, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	mux := server.ServeMux().(http.Handler)

	rec := post(mux, listRequest)
	if v, ok := tlvValue(rec.Body.Bytes(), tlvIdentifier); !ok || string(v) != "phone" {
		t.Errorf("list: body % x", rec.Body.Bytes())
	}

	// A Remove Pairing still reaches the library, which has no session for
	// this request and refuses it.
	rec = post(mux, removeRequest)
	if !bytes.Contains(rec.Body.Bytes(), []byte("-70401")) {
		t.Errorf("remove: body %q, want the library's refusal", rec.Body.String())
	}
	if _, err := store.Get(hex.EncodeToString([]byte("phone")) + ".pairing"); err != nil {
		t.Errorf("remove: pairing gone: %v", err)
	}

	routes := server.ServeMux().(chi.Routes).Routes()
	n := 0
	for _, r := range routes {
		if r.Pattern == pairingsPath {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d %s routes, want 1", n, pairingsPath)
	}
}

func TestListPairingsSkipsAnUnreadablePairing(t *testing.T) {
	store := hap.NewMemStore()
	savePairing(t, store, hap.Pairing{Name: "phone", PublicKey: []byte{1}, Permission: 1})
	if err := store.Set(hex.EncodeToString([]byte("torn"))+".pairing", []byte(`{"Name":`)); err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	rec := post(listPairingsHandler(next, store, func(*http.Request) bool { return true }, logr.Discard()), listRequest)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if v, _ := tlvValue(rec.Body.Bytes(), tlvIdentifier); string(v) != "phone" {
		t.Errorf("body % x, want the readable pairing", rec.Body.Bytes())
	}
}

func TestPairingsRejectsAnOversizedBody(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("oversized body reached the library") })
	body := append(append([]byte{}, removeRequest...), bytes.Repeat([]byte{0}, maxPairingsBody)...)
	rec := post(listPairingsHandler(next, hap.NewMemStore(), func(*http.Request) bool { return true }, logr.Discard()), body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d", rec.Code)
	}
}

func TestPairingsStaysPostOnly(t *testing.T) {
	store := hap.NewMemStore()
	server, err := hap.NewServer(store, accessory.NewBridge(accessory.Info{Name: "Test"}).A)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapPairings(server, store, func(*http.Request) bool { return true }, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		server.ServeMux().(http.Handler).ServeHTTP(rec, httptest.NewRequest(m, pairingsPath, bytes.NewReader(removeRequest)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, rec.Code)
		}
	}
}
