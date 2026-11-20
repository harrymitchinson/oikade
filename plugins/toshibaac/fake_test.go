package toshibaac

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// hubName is the fake hub's name, which its certificate, httptest's, holds.
const hubName = "example.com"

// wantDeviceID is the Device-ID the plugin sends for username: written out, so a change that would
// make the cloud see a new device at every login fails.
const wantDeviceID = "381292289610104d"

// fakeCloud is Toshiba's API and its hub, as the plugin uses them.
type fakeCloud struct {
	t      *testing.T
	server *httptest.Server
	hub    *mochi.Server
	// hubAddress is where the hub listens.
	hubAddress string

	mu       sync.Mutex
	password string
	// groups are the account's units, in its groups.
	groups [][]unit
	// outdoor is the home's OutdoorUnits.
	outdoor [][]string
	// states are what reading a unit's state gives, by its cloud ID.
	states map[string]string
	// access is the access token logged in with, and sas the hub's token registering gives.
	access, sas string
	// lifetime is how long a SAS token lasts, zero for one that doesn't say.
	lifetime time.Duration
	logins   int
	// down answers every request with 503, and refuseHub refuses the hub's logins.
	down, refuseHub bool
	// limited answers every request with this status, such as 403 or 429, and 0 answers them. Its
	// answer has the header Retry-After retryAfter, if it isn't empty, and the body limitedBody.
	limited                 int
	retryAfter, limitedBody string
	// bodies are the last body posted to each path.
	bodies map[string]map[string]string
	// connects are the hub's logins, as username and client ID.
	connects [][2]string
	// answers are the topics the plugin answered method calls on.
	answers []string
	// sent are the messages the plugin sent the cloud, as topic, QoS and payload, each recorded
	// before the hub acknowledges it.
	sent []string
	rid  int
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	c := &fakeCloud{t: t, password: password, states: map[string]string{}, bodies: map[string]map[string]string{}}
	c.server = httptest.NewTLSServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	c.startHub()
	return c
}

func (c *fakeCloud) set(f func(c *fakeCloud)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

// config is an account on this cloud.
func (c *fakeCloud) config() Config {
	roots := c.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return Config{
		Username: username, Password: plugin.NewSecret("toshiba"), OutdoorUnits: c.outdoor,
		api: c.server.URL, transport: c.server.Client().Transport,
		hub: hubOptions{address: c.hubAddress, tls: &tls.Config{RootCAs: roots, ServerName: hubName, MinVersion: tls.VersionTLS12}},
	}
}

func (c *fakeCloud) reply(w http.ResponseWriter, res any) {
	raw, err := json.Marshal(res)
	if err != nil {
		c.t.Error(err)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"IsSuccess": true, "Message": "Success", "StatusCode": "Success", "ResObj": json.RawMessage(raw)})
}

func (c *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.down:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case c.limited != 0:
		if c.retryAfter != "" {
			w.Header().Set("Retry-After", c.retryAfter)
		}
		w.WriteHeader(c.limited)
		_, _ = io.WriteString(w, c.limitedBody)
		return
	case r.Header.Get("Device-ID") == "":
		// As the real cloud has answered a login without one since 2026-07.
		c.t.Errorf("%s with no Device-ID", r.URL.Path)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case r.Header.Get("User-Agent") != "":
		// As the real cloud's firewall answers a browser's.
		c.t.Errorf("%s with User-Agent %q", r.URL.Path, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusForbidden)
		return
	case r.Header.Get("Device-ID") != wantDeviceID || r.Header.Get("Content-Type") != "application/json":
		c.t.Errorf("%s with Device-ID %q and Content-Type %q", r.URL.Path, r.Header.Get("Device-ID"), r.Header.Get("Content-Type"))
	}
	var body map[string]string
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			c.t.Errorf("%s posted %q: %v", r.URL.Path, raw, err)
		}
		c.bodies[r.URL.Path] = body
	}
	if r.URL.Path == loginPath {
		c.logins++
		if body["Username"] != username || body["Password"] != c.password {
			_ = json.NewEncoder(w).Encode(map[string]any{"IsSuccess": false, "StatusCode": badPassword, "Message": "Invalid Username or Password"})
			return
		}
		c.access = "access-" + strconv.Itoa(c.logins)
		c.reply(w, map[string]string{"access_token": c.access, "token_type": "Bearer", "consumerId": "consumer-1"})
		return
	}
	if c.access == "" || r.Header.Get("Authorization") != "Bearer "+c.access {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case registerPath:
		c.sas = fmt.Sprintf("SharedAccessSignature sr=%s%%2Fdevices%%2F%s&sig=sig-%d", hubName, url.QueryEscape(url.PathEscape(body["DeviceID"])), c.logins)
		if c.lifetime > 0 {
			c.sas += "&se=" + strconv.FormatInt(time.Now().Add(c.lifetime).Unix(), 10)
		}
		c.reply(w, map[string]string{"SasToken": c.sas})
	case mappingPath:
		if r.URL.Query().Get("consumerId") != "consumer-1" {
			c.t.Errorf("mapping for %q", r.URL.Query().Get("consumerId"))
		}
		groups := []map[string]any{}
		for i, g := range c.groups {
			groups = append(groups, map[string]any{"GroupId": strconv.Itoa(i), "GroupName": "Group " + strconv.Itoa(i), "ACList": g})
		}
		c.reply(w, groups)
	case statePath:
		state, ok := c.states[r.URL.Query().Get("ACId")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		c.reply(w, map[string]any{"ACStateData": state, "Fcu": map[string]string{"model_name": "RAS-B10"}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// count is how many logins there have been.
func (c *fakeCloud) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logins
}

func (c *fakeCloud) posted(path string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[path]
}

func (c *fakeCloud) hubLogins() [][2]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][2]string(nil), c.connects...)
}

func (c *fakeCloud) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

func (c *fakeCloud) answered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.answers...)
}

// startHub starts the hub: a broker on httptest's certificate that takes only the latest SAS token.
func (c *fakeCloud) startHub() {
	s := mochi.New(&mochi.Options{InlineClient: true, Logger: slog.New(slog.DiscardHandler)})
	if err := s.AddHook(&hubHook{cloud: c}, nil); err != nil {
		c.t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", c.server.TLS)
	if err != nil {
		c.t.Fatal(err)
	}
	c.hubAddress = ln.Addr().String()
	if err := s.AddListener(&hubListener{l: ln}); err != nil {
		c.t.Fatal(err)
	}
	if err := s.Subscribe("$iothub/methods/res/#", 1, func(_ *mochi.Client, _ packets.Subscription, pk packets.Packet) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.answers = append(c.answers, pk.TopicName+" "+string(pk.Payload))
	}); err != nil {
		c.t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	c.hub = s
	c.t.Cleanup(func() { _ = s.Close() })
}

// method calls the plugin's direct method as the hub does, and returns its request ID.
func (c *fakeCloud) method(name, cmd, source string, payload any) string {
	c.t.Helper()
	c.mu.Lock()
	c.rid++
	rid := strconv.Itoa(c.rid)
	c.mu.Unlock()
	raw, err := json.Marshal(map[string]any{
		"sourceId": source, "messageId": "0000000", "targetId": []string{username}, "cmd": cmd,
		"payload": payload, "timeStamp": "0000000",
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.hub.Publish("$iothub/methods/POST/"+name+"/?$rid="+rid, raw, false, 0); err != nil {
		c.t.Fatal(err)
	}
	return rid
}

// kick drops the plugin's connection to the hub.
func (c *fakeCloud) kick() {
	for _, cl := range c.hub.Clients.GetAll() {
		if cl.ID != mochi.InlineClientId {
			cl.Stop(errors.New("kicked"))
		}
	}
}

// hubHook takes a device whose password is the latest SAS token, with its username as IoT Hub has it.
type hubHook struct {
	mochi.HookBase
	cloud *fakeCloud
}

func (h *hubHook) ID() string { return "fake-iot-hub" }

func (h *hubHook) Provides(b byte) bool {
	return b == mochi.OnConnectAuthenticate || b == mochi.OnACLCheck || b == mochi.OnPublish
}

func (h *hubHook) OnConnectAuthenticate(cl *mochi.Client, pk packets.Packet) bool {
	c := h.cloud
	c.mu.Lock()
	defer c.mu.Unlock()
	user := string(pk.Connect.Username)
	c.connects = append(c.connects, [2]string{user, cl.ID})
	return !c.refuseHub && c.sas != "" && string(pk.Connect.Password) == c.sas && pk.ProtocolVersion == 4 &&
		user == hubName+"/"+cl.ID+"/?api-version="+hubAPIVersion
}

func (h *hubHook) OnACLCheck(*mochi.Client, string, bool) bool { return true }

// OnPublish records a device's message as the hub takes it. A subscriber would hear it only after
// the hub had sent the PUBACK, which a write returns on, so a test could look before it's there.
func (h *hubHook) OnPublish(_ *mochi.Client, pk packets.Packet) (packets.Packet, error) {
	if strings.HasPrefix(pk.TopicName, "devices/") {
		c := h.cloud
		c.mu.Lock()
		defer c.mu.Unlock()
		c.sent = append(c.sent, fmt.Sprintf("%s %d %s", pk.TopicName, pk.FixedHeader.Qos, pk.Payload))
	}
	return pk, nil
}

// hubListener serves an already bound TLS listener.
type hubListener struct {
	l net.Listener
}

var _ listeners.Listener = (*hubListener)(nil)

func (l *hubListener) Init(*slog.Logger) error { return nil }

func (l *hubListener) Serve(establish listeners.EstablishFn) {
	for {
		conn, err := l.l.Accept()
		if err != nil {
			return
		}
		go func() { _ = establish("hub", conn) }()
	}
}

func (l *hubListener) ID() string { return "hub" }

func (l *hubListener) Address() string { return l.l.Addr().String() }

func (l *hubListener) Protocol() string { return "tcp" }

func (l *hubListener) Close(closeClients listeners.CloseFn) {
	_ = l.l.Close()
	closeClients("hub")
}
