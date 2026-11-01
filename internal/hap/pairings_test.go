package hap

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/tlv8"
	"github.com/go-chi/chi"
)

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

// The library's own decoder fails on List Pairings. If brutella/hap fixes
// this, the wrapper can go.
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

	rec := post(listPairingsHandler(next, store, func(*http.Request) bool { return true }), listRequest)

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
	rec := post(listPairingsHandler(next, hap.NewMemStore(), func(*http.Request) bool { return true }), listRequest)
	if got, want := rec.Body.Bytes(), []byte{tlvState, 1, stateM2}; !bytes.Equal(got, want) {
		t.Errorf("body % x, want % x", got, want)
	}
}

func TestListPairingsNeedsAVerifiedController(t *testing.T) {
	store := hap.NewMemStore()
	savePairing(t, store, hap.Pairing{Name: "phone", PublicKey: []byte{1}, Permission: 1})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("List Pairings reached the library") })

	rec := post(listPairingsHandler(next, store, func(*http.Request) bool { return false }), listRequest)

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
		rec := post(listPairingsHandler(next, hap.NewMemStore(), func(*http.Request) bool { return true }), body)
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
	if err := fixListPairings(server, store); err != nil {
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
	if err := wrapPairings(server, store, func(*http.Request) bool { return true }); err != nil {
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
