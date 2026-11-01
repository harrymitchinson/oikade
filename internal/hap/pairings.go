package hap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/brutella/hap"
	"github.com/go-chi/chi"
)

// pairingsPath is the HAP endpoint for adding, removing and listing pairings.
const pairingsPath = "/pairings"

// TLV types and values from the HAP specification (section 5.15).
const (
	tlvMethod     byte = 0x00
	tlvIdentifier byte = 0x01
	tlvPublicKey  byte = 0x03
	tlvState      byte = 0x06
	tlvPermission byte = 0x0b
	tlvSeparator  byte = 0xff
	stateM2       byte = 0x02
)

// maxPairingsBody bounds a /pairings request. Real ones are under 200 bytes.
const maxPairingsBody = 64 << 10

// fixListPairings makes List Pairings work on server.
//
// brutella/hap v0.0.35 decodes every /pairings request into one struct whose
// Identifier is required. List Pairings carries only a method and a state, so
// the library answers it with an error, and its list branch never runs. The
// Home app lists a Bridge's pairings before it adds the home hub, so without
// this the hub never pairs (OIK-75). The wrapper answers List Pairings itself,
// from the pairings in store, and passes every other request to the library
// unchanged.
func fixListPairings(server *hap.Server, store hap.Store) error {
	return wrapPairings(server, store, server.IsAuthorized)
}

// wrapPairings replaces server's /pairings route with listPairingsHandler.
func wrapPairings(server *hap.Server, store hap.Store, authorized func(*http.Request) bool) error {
	routes, ok := server.ServeMux().(chi.Routes)
	if !ok {
		return errors.New("HAP server's mux has no routes to read")
	}
	var original http.Handler
	for _, r := range routes.Routes() {
		if r.Pattern == pairingsPath {
			original = r.Handlers[http.MethodPost]
		}
	}
	if original == nil {
		return fmt.Errorf("HAP server has no POST %s route", pairingsPath)
	}
	server.ServeMux().Handle(pairingsPath, listPairingsHandler(original, store, authorized))
	return nil
}

// listPairingsHandler answers List Pairings from store for an authorized
// (pair-verified) controller, and passes every other request to next.
//
// The HAP specification only lets admin controllers list pairings. brutella/hap
// doesn't export which controller a session belongs to, so this lets any
// verified controller list them. A pairing's identifier and public key aren't
// secret, and only controllers the home has paired get this far.
func listPairingsHandler(next http.Handler, store hap.Store, authorized func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPairingsBody))
		if err != nil {
			http.Error(w, "reading request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if method, ok := tlvValue(body, tlvMethod); !ok || len(method) != 1 || method[0] != hap.MethodListPairings {
			next.ServeHTTP(w, r)
			return
		}
		if !authorized(r) {
			_ = hap.JsonError(w, hap.JsonStatusInsufficientPrivileges)
			return
		}
		pairings, err := storedPairings(store)
		if err != nil {
			w.Header().Set("Content-Type", hap.HTTPContentTypePairingTLV8)
			_, _ = w.Write(appendTLV(appendTLV(nil, tlvState, []byte{stateM2}), 0x07, []byte{hap.TlvErrorUnknown}))
			return
		}
		w.Header().Set("Content-Type", hap.HTTPContentTypePairingTLV8)
		_, _ = w.Write(encodePairingList(pairings))
	})
}

// storedPairings reads the pairings brutella/hap keeps in store, as
// "<hex name>.pairing" keys holding JSON, sorted by name.
func storedPairings(store hap.Store) ([]hap.Pairing, error) {
	keys, err := store.KeysWithSuffix(".pairing")
	if err != nil {
		return nil, err
	}
	pairings := make([]hap.Pairing, 0, len(keys))
	for _, k := range keys {
		b, err := store.Get(k)
		if err != nil {
			return nil, err
		}
		var p hap.Pairing
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("decoding pairing %s: %w", k, err)
		}
		pairings = append(pairings, p)
	}
	slices.SortFunc(pairings, func(a, b hap.Pairing) int { return strings.Compare(a.Name, b.Name) })
	return pairings, nil
}

// encodePairingList is a List Pairings response: the state M2, then each
// pairing's identifier, public key and permission, with a separator between
// pairings.
func encodePairingList(pairings []hap.Pairing) []byte {
	b := appendTLV(nil, tlvState, []byte{stateM2})
	for i, p := range pairings {
		if i > 0 {
			b = appendTLV(b, tlvSeparator, nil)
		}
		b = appendTLV(b, tlvIdentifier, []byte(p.Name))
		b = appendTLV(b, tlvPublicKey, p.PublicKey)
		b = appendTLV(b, tlvPermission, []byte{p.Permission})
	}
	return b
}

// appendTLV appends one TLV8 item, split into 255-byte fragments as the
// specification requires for longer values.
func appendTLV(b []byte, typ byte, value []byte) []byte {
	for {
		n := min(len(value), 255)
		b = append(b, typ, byte(n))
		b = append(b, value[:n]...)
		value = value[n:]
		if len(value) == 0 {
			return b
		}
	}
}

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
