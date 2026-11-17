package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metrics"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

func TestAPI(t *testing.T) {
	s := &apiServer{log: slog.New(slog.DiscardHandler), ctx: context.Background()}
	server := httptest.NewServer(s.routes())
	defer server.Close()

	cases := []struct {
		name, method, path, body string
		status                   int
		says                     string
	}{
		{"a path outside the API names it", http.MethodGet, "/v2/status", "", http.StatusNotFound, "this host speaks /v1"},
		{"an unknown route names it", http.MethodPost, "/v1/nothing", "", http.StatusNotFound, "this host speaks /v1"},
		{"the wrong method is no route", http.MethodPost, "/v1/status", "", http.StatusNotFound, "this host speaks /v1"},
		{"a body that isn't JSON", http.MethodPost, "/v1/set", "{", http.StatusBadRequest, "reading the request"},
		{"a refused call", http.MethodPost, "/v1/secrets/set", `{"secret":"../x"}`, http.StatusUnprocessableEntity, ""},
		{"a call with no body", http.MethodGet, "/v1/info", "", http.StatusOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, server.URL+c.path, strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var answer api.Response
			if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != c.status {
				t.Errorf("status %d, want %d: %+v", resp.StatusCode, c.status, answer)
			}
			if !strings.Contains(answer.Error, c.says) {
				t.Errorf("error %q doesn't say %q", answer.Error, c.says)
			}
			if got := resp.Header.Get("Content-Type"); got != api.ContentType {
				t.Errorf("content type %q", got)
			}
		})
	}
}

func TestEveryRequestTypeHasOneRoute(t *testing.T) {
	seen := map[string]bool{}
	paths := map[string]bool{}
	for _, r := range api.Routes {
		if seen[r.Type] {
			t.Errorf("%s has two routes", r.Type)
		}
		if paths[r.Method+r.Path] {
			t.Errorf("%s %s is two routes", r.Method, r.Path)
		}
		seen[r.Type], paths[r.Method+r.Path] = true, true
		if !strings.HasPrefix(r.Path, api.Prefix+"/") {
			t.Errorf("%s is outside %s", r.Path, api.Prefix)
		}
	}
}

// publicRoutes are the only calls the network API takes without a token. It's written out here, not
// read from api.Routes, so that a route made public by mistake fails this test.
var publicRoutes = map[string]bool{api.TypeLogin: true}

func TestEveryRouteButLoginNeedsAToken(t *testing.T) {
	c, err := loadClients(openAPIStore(t))
	if err != nil {
		t.Fatal(err)
	}
	s := &apiServer{
		log: slog.New(slog.DiscardHandler), ctx: context.Background(), clients: c,
		authFailures: metrics.NewSet().NewCounter("auth_failures_total"),
	}
	server := httptest.NewServer(s.networkRoutes())
	defer server.Close()
	for _, r := range api.Routes {
		t.Run(r.Type, func(t *testing.T) {
			if r.Public != publicRoutes[r.Type] {
				t.Fatalf("public is %v, want %v", r.Public, publicRoutes[r.Type])
			}
			for _, token := range []string{"", "oik_nothing"} {
				req, err := http.NewRequest(r.Method, server.URL+r.Path, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if refused := resp.StatusCode == http.StatusUnauthorized; refused == publicRoutes[r.Type] {
					t.Errorf("token %q: %d", token, resp.StatusCode)
				}
			}
		})
	}
}
