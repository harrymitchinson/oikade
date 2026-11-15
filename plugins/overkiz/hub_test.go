package overkiz

import (
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

const enduserAPI = "/enduser-mobile-web/1/enduserAPI"

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
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	raw, err := os.ReadFile("testdata/local-setup.json")
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{t: t, token: token, listeners: map[string][]map[string]any{}, requests: map[string]int{}}
	if err := json.Unmarshal(raw, &h.setup); err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewTLSServer(h)
	t.Cleanup(h.server.Close)
	return h
}

// address is the hub's host and port, for Local.
func (h *fakeHub) address() string { return strings.TrimPrefix(h.server.URL, "https://") }

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, enduserAPI)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
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
	case r.Header.Get("Authorization") != "Bearer "+h.token:
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

func (h *fakeHub) executions() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.executed...)
}
