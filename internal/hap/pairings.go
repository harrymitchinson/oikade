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
	"github.com/brutella/hap/tlv8"
	"github.com/go-chi/chi"
	"github.com/go-logr/logr"
)

// pairingsPath is the HAP endpoint for adding, removing and listing pairings.
const pairingsPath = "/pairings"

// TLV types and values from the HAP specification (section 5.15).
const (
	tlvIdentifier byte = 0x01
	tlvPublicKey  byte = 0x03
	tlvState      byte = 0x06
	tlvError      byte = 0x07
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
func fixListPairings(server *hap.Server, store hap.Store, log logr.Logger) error {
	return wrapPairings(server, store, server.IsAuthorized, log)
}

// wrapPairings replaces server's POST /pairings route with listPairingsHandler.
// It relies on brutella/hap routing with chi, and fails without changing
// anything if that or the route has gone.
func wrapPairings(server *hap.Server, store hap.Store, authorized func(*http.Request) bool, log logr.Logger) error {
	router, ok := server.ServeMux().(chi.Router)
	if !ok {
		return errors.New("HAP server's mux isn't a chi router")
	}
	var original http.Handler
	for _, r := range router.Routes() {
		if r.Pattern == pairingsPath {
			original = r.Handlers[http.MethodPost]
		}
	}
	if original == nil {
		return fmt.Errorf("HAP server has no POST %s route", pairingsPath)
	}
	router.Post(pairingsPath, listPairingsHandler(original, store, authorized, log).ServeHTTP)
	return nil
}

// listPairingsHandler answers List Pairings from store for an authorized
// (pair-verified) controller, and passes every other request to next.
//
// The HAP specification only lets admin controllers list pairings. brutella/hap
// doesn't export which controller a session belongs to, so this lets any
// verified controller list them. A pairing's identifier and public key aren't
// secret, and only controllers the home has paired get this far.
func listPairingsHandler(next http.Handler, store hap.Store, authorized func(*http.Request) bool, log logr.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPairingsBody+1))
		if err != nil {
			http.Error(w, "reading request", http.StatusBadRequest)
			return
		}
		if len(body) > maxPairingsBody {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !isListPairings(body) {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", hap.HTTPContentTypePairingTLV8)
		if !authorized(r) {
			_ = hap.JsonError(w, hap.JsonStatusInsufficientPrivileges)
			return
		}
		pairings, err := storedPairings(store, log)
		if err != nil {
			log.Error(err, "Could not read HomeKit pairings for List Pairings")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(appendTLV(appendTLV(nil, tlvState, []byte{stateM2}), tlvError, []byte{hap.TlvErrorUnknown}))
			return
		}
		_, _ = w.Write(encodePairingList(pairings))
	})
}

// isListPairings reports whether body is a List Pairings request.
func isListPairings(body []byte) bool {
	var m struct {
		Method byte `tlv8:"0"`
	}
	return tlv8.Unmarshal(body, &m) == nil && m.Method == hap.MethodListPairings
}

// storedPairings reads the pairings brutella/hap keeps in store, as
// "<hex name>.pairing" keys holding JSON, sorted by name. Like the library, it
// skips a pairing it can't read, so one bad file doesn't hide the rest.
func storedPairings(store hap.Store, log logr.Logger) ([]hap.Pairing, error) {
	keys, err := store.KeysWithSuffix(".pairing")
	if err != nil {
		return nil, err
	}
	pairings := make([]hap.Pairing, 0, len(keys))
	for _, k := range keys {
		var p hap.Pairing
		b, err := store.Get(k)
		if err == nil {
			err = json.Unmarshal(b, &p)
		}
		if err != nil {
			log.Error(err, "Skipped unreadable HomeKit pairing", "key", k)
			continue
		}
		pairings = append(pairings, p)
	}
	slices.SortFunc(pairings, func(a, b hap.Pairing) int { return strings.Compare(a.Name, b.Name) })
	return pairings, nil
}

// encodePairingList is a List Pairings response: the state M2, then each
// pairing's identifier, public key and permission, with a separator between
// pairings. brutella/hap's tlv8 encoder can't write separators, so this is
// written by hand.
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
