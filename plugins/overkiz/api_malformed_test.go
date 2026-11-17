package overkiz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnAnswerThatIsntJSONIsAnError: a hub or the cloud answering with something that isn't the
// JSON asked for, or isn't JSON at all, fails the call rather than reading as empty, and an error
// page that isn't JSON is still an apiError with its status.
func TestAnAnswerThatIsntJSONIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"html":         {http.StatusOK, "<html>maintenance</html>"},
		"truncated":    {http.StatusOK, `[{"deviceURL":"io://1234-5678-9012/1"`},
		"wrong type":   {http.StatusOK, `{"deviceURL":"io://1234-5678-9012/1"}`},
		"invalid utf8": {http.StatusOK, "[\"\xff\xfe\"]"},
		"error page":   {http.StatusBadGateway, "<html>bad gateway</html>"},
		"error object": {http.StatusBadRequest, `{"errorCode":`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := &client{http: server.Client(), base: server.URL + "/enduserAPI"}
			devices, err := c.devices(context.Background())
			if err == nil {
				t.Fatalf("got %+v, no error", devices)
			}
			var api *apiError
			if isAPI := errors.As(err, &api); isAPI != (tc.status != http.StatusOK) {
				t.Errorf("%v: apiError %v", err, isAPI)
			} else if isAPI && api.Status != tc.status {
				t.Errorf("status %d, want %d", api.Status, tc.status)
			} else if !isAPI && !strings.Contains(err.Error(), "/setup/devices") {
				t.Errorf("%v doesn't say what it was reading", err)
			}
		})
	}
}
