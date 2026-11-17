package overkiz

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// The enduser API's root on a hub, and on the cloud.
const (
	enduserAPI = "/enduser-mobile-web/1/enduserAPI"
	cloudAPI   = "/enduser-mobile-web/enduserAPI"
)

// fakeHub is a TaHoma hub's local API, from testdata/local-setup.json. It answers as Overkiz does, as
// far as pyoverkiz and Somfy's Developer Mode docs say: the setup, event listeners and executions.
type fakeHub struct {
	t      *testing.T
	server *httptest.Server
	token  string

	mu sync.Mutex
	// setup is what /setup/devices answers.
	setup []map[string]any
	// listeners are the registered listeners and their events.
	listeners map[string][]map[string]any
	next      int
	// executed are the bodies of /exec/apply.
	executed []map[string]any
	// down answers every request with a 503, and limited with a 429.
	down, limited bool
	// requests counts requests by path, without the listener's ID.
	requests map[string]int

	// cloud is whether it's the Cozytouch cloud, which logs in with password, and keeps sessions by
	// cookie, rather than a hub that takes a token.
	cloud    bool
	password string
	// clientRefused refuses the app's client ID, as Atlantic would once it changes it.
	clientRefused bool
	sessions      map[string]bool
	// endAtFetch ends each session at its first fetch of events, as a cloud that ends them as they
	// begin does, once the plugin has connected.
	endAtFetch bool
	// tokens are Atlantic's tokens, and whether each is still good.
	tokens map[string]bool
	// noOverkizAccount answers Overkiz's login as for an account that's only on Magellan.
	noOverkizAccount bool

	// magellan is Magellan's setup, nil for an account it has no setup for, and magellanDown
	// answers it with a 503.
	magellan     []map[string]any
	magellanDown bool
	// written are the bodies of Magellan's writecapability.
	written []map[string]any

	// ca is the authority that signed a hub's certificate, standing in for Overkiz's.
	ca *x509.CertPool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	return newFake(t, "testdata/local-setup.json", false)
}

// newFakeCloud is the Cozytouch cloud, from testdata/cozytouch-setup.json: Atlantic's login and
// Overkiz's API in one server, as pyoverkiz has them.
func newFakeCloud(t *testing.T) *fakeHub {
	t.Helper()
	return newFake(t, "testdata/cozytouch-setup.json", true)
}

func newFake(t *testing.T, setup string, cloud bool) *fakeHub {
	t.Helper()
	raw, err := os.ReadFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{
		t: t, token: token, listeners: map[string][]map[string]any{}, requests: map[string]int{},
		cloud: cloud, password: password, sessions: map[string]bool{}, tokens: map[string]bool{},
	}
	if err := json.Unmarshal(raw, &h.setup); err != nil {
		t.Fatal(err)
	}
	if cloud {
		h.server = httptest.NewTLSServer(h)
	} else {
		// A hub's certificate, from an authority of its own as Overkiz's, names the hub in its CN only.
		h.ca, h.server = newHubServer(t, h, hubName, nil)
	}
	t.Cleanup(h.server.Close)
	return h
}

// address is the hub's host and port, for Local.
func (h *fakeHub) address() string { return strings.TrimPrefix(h.server.URL, "https://") }

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := enduserAPI
	if h.cloud {
		prefix = cloudAPI
		if h.login(w, r) {
			return
		}
	}
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	counted := path
	if parts := strings.Split(path, "/"); len(parts) == 4 && parts[1] == "events" && parts[2] != "register" {
		counted = "/events/*/" + parts[3]
	}
	h.requests[counted]++
	answer := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case h.down:
		answer(http.StatusServiceUnavailable, map[string]string{"errorCode": "UNSPECIFIED_ERROR", "error": "Service unavailable"})
		return
	case h.limited:
		answer(http.StatusTooManyRequests, map[string]string{"errorCode": "UNSPECIFIED_ERROR", "error": "Too many requests, try again later"})
		return
	case h.cloud && !h.signedIn(r):
		answer(http.StatusUnauthorized, map[string]string{"errorCode": "RESOURCE_ACCESS_DENIED", "error": "Not authenticated"})
		return
	case !h.cloud && r.Header.Get("Authorization") != "Bearer "+h.token:
		answer(http.StatusUnauthorized, map[string]string{"errorCode": "RESOURCE_ACCESS_DENIED", "error": "Not authenticated"})
		return
	}
	switch parts := strings.Split(path, "/"); {
	case r.Method == http.MethodGet && path == "/setup/devices":
		answer(http.StatusOK, h.setup)
	case r.Method == http.MethodPost && path == "/events/register":
		h.next++
		id := fmt.Sprintf("listener-%d", h.next)
		h.listeners[id] = []map[string]any{}
		answer(http.StatusOK, map[string]string{"id": id})
	case r.Method == http.MethodPost && len(parts) == 4 && parts[1] == "events":
		events, ok := h.listeners[parts[2]]
		if !ok {
			answer(http.StatusBadRequest, map[string]string{"errorCode": "UNSPECIFIED_ERROR", "error": "No registered event listener"})
			return
		}
		switch parts[3] {
		case "fetch":
			if c, err := r.Cookie("JSESSIONID"); err == nil && h.endAtFetch {
				h.sessions[c.Value] = false
				answer(http.StatusUnauthorized, map[string]string{"errorCode": "RESOURCE_ACCESS_DENIED", "error": "Not authenticated"})
				return
			}
			h.listeners[parts[2]] = []map[string]any{}
			answer(http.StatusOK, events)
		case "unregister":
			delete(h.listeners, parts[2])
			answer(http.StatusOK, map[string]any{})
		}
	case r.Method == http.MethodPost && path == "/exec/apply":
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			h.t.Errorf("exec body: %s", raw)
		}
		h.executed = append(h.executed, body)
		exec := fmt.Sprintf("exec-%d", len(h.executed))
		answer(http.StatusOK, map[string]string{"execId": exec})
	default:
		http.NotFound(w, r)
	}
}

// event queues an event for every listener.
func (h *fakeHub) event(e map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.listeners {
		h.listeners[id] = append(h.listeners[id], e)
	}
}

// closure queues a device's new core:ClosureState.
func (h *fakeHub) closure(url string, v int) {
	h.event(map[string]any{
		"name": eventStateChanged, "deviceURL": url,
		"deviceStates": []map[string]any{{"name": "core:ClosureState", "type": 1, "value": v}},
	})
}

// forget drops every listener, as the hub does with one nobody has fetched for a while.
func (h *fakeHub) forget() {
	h.mu.Lock()
	defer h.mu.Unlock()
	clear(h.listeners)
}

func (h *fakeHub) set(f func(h *fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func (h *fakeHub) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests[path]
}

// total counts every request.
func (h *fakeHub) total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.requests {
		n += c
	}
	return n
}

func (h *fakeHub) executions() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.executed...)
}

// login answers Atlantic's token and JWT calls and Overkiz's login, and says whether it did.
func (h *fakeHub) login(w http.ResponseWriter, r *http.Request) bool {
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	switch r.URL.Path {
	case "/token":
		h.requests["/token"]++
		switch {
		case h.down:
			answer(http.StatusServiceUnavailable, `{"error":"unavailable"}`)
		case h.clientRefused:
			answer(http.StatusUnauthorized, `{"error":"invalid_client","error_description":"Client authentication failed"}`)
		case r.Method != http.MethodPost || r.Header.Get("Authorization") != "Basic "+cozytouchClientID ||
			r.FormValue("grant_type") != "password" || r.FormValue("username") != "GA-PRIVATEPERSON/someone@example.com":
			h.t.Errorf("token request: %s %v %v", r.Method, r.Header, r.Form)
			answer(http.StatusBadRequest, `{"error":"invalid_request"}`)
		case r.FormValue("password") != h.password:
			answer(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Provided Authorization Grant is invalid."}`)
		default:
			issued := fmt.Sprintf("atlantic-token-%d", len(h.tokens)+1)
			h.tokens[issued] = true
			answer(http.StatusOK, `{"access_token":"`+issued+`","token_type":"bearer","expires_in":28800}`)
		}
	case "/magellan/accounts/jwt":
		if !h.tokenGood(r) {
			answer(http.StatusUnauthorized, `{"error":"invalid_token"}`)
			return true
		}
		answer(http.StatusOK, `"the-jwt"`)
	case cloudAPI + "/login":
		h.requests["/login"]++
		if h.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "<html>Server is down for maintenance</html>")
			return true
		}
		if r.FormValue("jwt") != "the-jwt" {
			answer(http.StatusUnauthorized, `{"errorCode":"AUTHENTICATION_ERROR","error":"Bad credentials"}`)
			return true
		}
		if h.noOverkizAccount {
			answer(http.StatusUnauthorized, `{"errorCode":"AUTHENTICATION_ERROR","error":"No such user account : GACOMA_Production_1234567"}`)
			return true
		}
		session := fmt.Sprintf("session-%d", len(h.sessions)+1)
		h.sessions[session] = true
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: session, Path: "/"})
		answer(http.StatusOK, `{"success":true,"roles":[{"name":"ENDUSER"}]}`)
	default:
		if strings.HasPrefix(r.URL.Path, "/magellan/") {
			h.answerMagellan(w, r)
			return true
		}
		return false
	}
	return true
}

func (h *fakeHub) tokenGood(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && h.tokens[token]
}

// answerMagellan answers Magellan's setup, writes and executions, as a real account's did. A write
// changes the setup at once, and its execution is done.
func (h *fakeHub) answerMagellan(w http.ResponseWriter, r *http.Request) {
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	h.requests[r.URL.Path]++
	switch {
	case h.magellanDown:
		answer(http.StatusServiceUnavailable, `{"message":"unavailable"}`)
	case !h.tokenGood(r):
		answer(http.StatusUnauthorized, `{"code":"900901","message":"Invalid Credentials","description":"Access failure"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/magellan/cozytouch/setupviewv2":
		if h.magellan == nil {
			answer(http.StatusNotFound, `{"code":43000404,"message":"Setup not found","type":"SetupNotFound"}`)
			return
		}
		raw, _ := json.Marshal(h.magellan)
		answer(http.StatusOK, string(raw))
	case r.Method == http.MethodPost && r.URL.Path == "/magellan/executions/writecapability":
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			h.t.Errorf("writecapability body: %s", raw)
		}
		if !h.writeMagellan(body) {
			answer(http.StatusForbidden, `{"code":43000011,"message":"Access to resource is forbidden for provided access_token.","type":"ResourceAccessForbidden"}`)
			return
		}
		h.written = append(h.written, body)
		answer(http.StatusCreated, fmt.Sprint(900000+len(h.written)))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/magellan/executions/"):
		id := strings.TrimPrefix(r.URL.Path, "/magellan/executions/")
		answer(http.StatusOK, `{"id":`+id+`,"state":3,"executionId":`+id+`,"executionState":3,"details":"execution Done"}`)
	default:
		http.NotFound(w, r)
	}
}

// writeMagellan sets a capability of a device in the setup, and says whether it found the device.
func (h *fakeHub) writeMagellan(body map[string]any) bool {
	for _, setup := range h.magellan {
		devices, _ := setup["devices"].([]any)
		for _, d := range devices {
			dev, _ := d.(map[string]any)
			if fmt.Sprint(dev["deviceId"]) != fmt.Sprint(body["deviceId"]) {
				continue
			}
			caps, _ := dev["capabilities"].([]any)
			for _, c := range caps {
				cp, _ := c.(map[string]any)
				if fmt.Sprint(cp["capabilityId"]) == fmt.Sprint(body["capabilityId"]) {
					cp["value"] = body["value"]
					return true
				}
			}
			dev["capabilities"] = append(caps, map[string]any{"capabilityId": body["capabilityId"], "value": body["value"]})
			return true
		}
	}
	return false
}

// withMagellan gives the account Magellan's setup from testdata/magellan-setup.json.
func (h *fakeHub) withMagellan() *fakeHub {
	h.t.Helper()
	raw, err := os.ReadFile("testdata/magellan-setup.json")
	if err != nil {
		h.t.Fatal(err)
	}
	h.set(func(h *fakeHub) {
		if err := json.Unmarshal(raw, &h.magellan); err != nil {
			h.t.Fatal(err)
		}
	})
	return h
}

// magellanWrites are the bodies of Magellan's writecapability.
func (h *fakeHub) magellanWrites() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.written...)
}

// magellanDevice changes a device in Magellan's setup.
func (h *fakeHub) magellanDevice(id float64, f func(dev map[string]any)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, setup := range h.magellan {
		devices, _ := setup["devices"].([]any)
		for _, d := range devices {
			if dev, _ := d.(map[string]any); dev["deviceId"] == id {
				f(dev)
			}
		}
	}
}

// expireTokens ends every Atlantic token, as eight hours do.
func (h *fakeHub) expireTokens() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for t := range h.tokens {
		h.tokens[t] = false
	}
}

func (h *fakeHub) signedIn(r *http.Request) bool {
	c, err := r.Cookie("JSESSIONID")
	return err == nil && h.sessions[c.Value]
}

// expire ends every session, as the cloud does after a while.
func (h *fakeHub) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		h.sessions[s] = false
	}
}

// roots trusts the fake's certificate, as the system trusts the real cloud's.
func (h *fakeHub) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(h.server.Certificate())
	return pool
}
