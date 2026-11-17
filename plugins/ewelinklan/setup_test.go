package ewelinklan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

const (
	testAppID     = "app-7f3a"
	testAppSecret = "app-secret-91c2"
	testToken     = "token-55e0"
	testCode      = "code-12ab"
	neighbourID   = "1000cafe01"
)

// fakeEWeLink is eWeLink's API, as the docs describe it. Not recorded from the real one.
type fakeEWeLink struct {
	t      *testing.T
	server *httptest.Server
	// things is the answer to the device list.
	things string

	mu       sync.Mutex
	tokens   int
	listings int
}

func newFakeEWeLink(t *testing.T, things string) *fakeEWeLink {
	t.Helper()
	f := &fakeEWeLink{t: t, things: things}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/user/oauth/token", f.token)
	mux.HandleFunc("GET /v2/device/thing", f.list)
	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeEWeLink) cloud() *cloud {
	return &cloud{
		signIn: f.server.URL + "/oauth/index.html",
		api:    func(string) string { return f.server.URL },
		client: f.server.Client(),
	}
}

func (f *fakeEWeLink) token(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a := app{id: testAppID, secret: testAppSecret}
	var req map[string]string
	_ = json.Unmarshal(body, &req)
	switch {
	case r.Header.Get("X-CK-Appid") != testAppID || r.Header.Get("X-CK-Nonce") == "":
		_, _ = io.WriteString(w, `{"error":407,"msg":"bad appid"}`)
	case r.Header.Get("Authorization") != "Sign "+a.sign(body):
		_, _ = io.WriteString(w, `{"error":401,"msg":"wrong sign"}`)
	case req["code"] != testCode || req["redirectUrl"] != redirect || req["grantType"] != "authorization_code":
		_, _ = io.WriteString(w, `{"error":400,"msg":"bad code"}`)
	default:
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"error":0,"msg":"","data":{"accessToken":"`+testToken+`","atExpiredTime":1}}`)
	}
}

func (f *fakeEWeLink) list(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken || r.URL.Query().Get("num") != "0" {
		_, _ = io.WriteString(w, `{"error":401,"msg":"token invalid"}`)
		return
	}
	f.mu.Lock()
	f.listings++
	f.mu.Unlock()
	_, _ = io.WriteString(w, `{"error":0,"msg":"","data":{"thingList":`+f.things+`,"total":3}}`)
}

const things = `[
	{"itemType":1,"itemData":{"deviceid":"1000AABBCC","devicekey":"` + fixtureKey + `","name":"Greenhouse heater"}},
	{"itemType":1,"itemData":{"deviceid":"1000ddeeff","devicekey":"key-of-the-probe","name":""}},
	{"itemType":3,"itemData":{"name":"Downstairs"}}
]`

// person answers a setup as someone at oikade setup would, and as the host does.
type person struct {
	answers map[string]string
	// browse follows the sign-in page, and returns the query it redirects with.
	browse func(page string) string
}

// signIn follows the sign-in page as eWeLink's would, once you've signed in. Its signature is checked
// at the token, as eWeLink's is.
func signIn(t *testing.T, region string) func(string) string {
	t.Helper()
	return func(page string) string {
		u, err := url.Parse(page)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("clientId") != testAppID || q.Get("authorization") == "" || q.Get("seq") == "" || q.Get("nonce") == "" ||
			q.Get("redirectUrl") != redirect || q.Get("grantType") != "authorization_code" {
			t.Errorf("sign-in page: %s", page)
		}
		return url.Values{"code": {testCode}, "region": {region}, "state": {q.Get("state")}}.Encode()
	}
}

// setup runs the plugin's setup as the host would, and returns its steps.
func (b *bench) setup(p person) []contractv1.SetupStep {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c, err := mqtt.Connect(ctx, b.broker, "ewelink-setup")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(context.Background()) }()
	got := make(chan contractv1.SetupStep, 64)
	if _, err := c.Subscribe(ctx, root.SetupOut(), func(m mqtt.Message) {
		var s contractv1.SetupStep
		if json.Unmarshal(m.Payload, &s) == nil {
			got <- s
		}
	}); err != nil {
		b.t.Fatal(err)
	}
	send := func(r contractv1.SetupRequest) {
		raw, _ := json.Marshal(r)
		if err := c.Publish(ctx, root.SetupIn(), raw, false); err != nil {
			b.t.Fatal(err)
		}
	}
	send(contractv1.SetupRequest{Session: "s", Start: true})
	var steps []contractv1.SetupStep
	for {
		select {
		case s := <-got:
			steps = append(steps, s)
			switch {
			case s.Done != nil:
				return steps
			case s.Ask != nil:
				answer, ok := p.answers[s.Ask.Prompt]
				if !ok {
					b.t.Fatalf("unexpected question %q", s.Ask.Prompt)
				}
				send(contractv1.SetupRequest{Session: "s", Reply: s.Seq, Value: answer})
			case s.Open != nil:
				send(contractv1.SetupRequest{Session: "s", Reply: s.Seq, Value: p.browse(s.Open.URL)})
			case s.Keep != nil:
				b.secret(s.Keep.Name, s.Keep.Value)
				send(contractv1.SetupRequest{Session: "s", Reply: s.Seq})
			}
		case <-ctx.Done():
			b.t.Fatalf("setup didn't finish: %+v", steps)
		}
	}
}

func kept(steps []contractv1.SetupStep) []string {
	var out []string
	for _, s := range steps {
		if s.Keep != nil {
			out = append(out, s.Keep.Name)
		}
	}
	return out
}

func said(steps []contractv1.SetupStep) string {
	var out []string
	for _, s := range steps {
		if s.Say != "" {
			out = append(out, s.Say)
		}
	}
	return strings.Join(out, "\n")
}

func TestSetupFetchesEachDevicesLANKeyAndTheDeviceComesUp(t *testing.T) {
	cloud := newFakeEWeLink(t, things)
	b := startWith(t, Config{}, fast, cloud.cloud())
	d := b.device(relayID, "th_plug", relayOf("on"))
	d.report()
	// A neighbour's, which isn't in the account.
	b.device(neighbourID, "th_plug", relayOf("off")).report()
	b.eventually("setup to be needed", b.needsSetup)

	a := app{id: testAppID, secret: testAppSecret}
	steps := b.setup(person{
		answers: map[string]string{"Its app ID": testAppID, "Its app secret": testAppSecret},
		browse: func(page string) string {
			u, _ := url.Parse(page)
			if q := u.Query(); q.Get("authorization") != a.sign([]byte(testAppID+"_"+q.Get("seq"))) {
				t.Errorf("sign-in page isn't signed with the app secret: %s", page)
			}
			return signIn(t, "eu")(page)
		},
	})
	done := steps[len(steps)-1].Done
	if done.Error != "" || done.Result != "ewelink is set up: 2 LAN keys kept, 0 already up to date" {
		t.Fatalf("done: %+v\n%s", done, said(steps))
	}
	if got := strings.Join(kept(steps), ","); got != "ewelink-app-id,ewelink-app-secret,1000aabbcc,1000ddeeff" {
		t.Errorf("kept %s", got)
	}
	for _, s := range steps {
		if s.Ask != nil && s.Ask.Prompt == "Its app secret" && !s.Ask.Secret {
			t.Error("The app secret should be asked for as a secret")
		}
	}
	if want := `1000aabbcc is "Greenhouse heater" in the eWeLink app: ` +
		`home.Device("greenhouse-heater").Hardware("ewelink", "1000aabbcc") in your home's code names it so`; !strings.Contains(said(steps), want) {
		t.Errorf("said:\n%s", said(steps))
	}
	// The probe isn't on this network, so there's nothing to name.
	if strings.Contains(said(steps), probeID) {
		t.Errorf("said:\n%s", said(steps))
	}
	// The key is read on the next sweep, with no new report.
	b.waitAvailability(relayID, contractv1.Online)
	// The neighbour's still has no key, which is its own problem, not the plugin's.
	b.eventually("setup no longer needed", func() bool { return !b.needsSetup() })
	if got := b.reports()[neighbourID].Error; got != "no LAN key: oikade setup ewelink" {
		t.Errorf("neighbour's: %q", got)
	}
}

func TestSetupAgainAsksNothingAndChangesNothing(t *testing.T) {
	cloud := newFakeEWeLink(t, things)
	b := startWith(t, Config{}, fast, cloud.cloud())
	b.secret(appIDSecret, testAppID+"\n")
	b.secret(appSecretSecret, testAppSecret)
	b.secret(relayID, fixtureKey)
	b.secret(probeID, "an old key")

	// No answers: any question fails the test.
	steps := b.setup(person{browse: signIn(t, "us")})
	if done := steps[len(steps)-1].Done; done.Result != "ewelink is set up: 1 LAN key kept, 1 already up to date" {
		t.Fatalf("done: %+v", done)
	}
	if got := strings.Join(kept(steps), ","); got != probeID {
		t.Errorf("kept %s, want only the key that changed", got)
	}
	raw, _ := os.ReadFile(filepath.Join(b.secrets, probeID))
	if string(raw) != "key-of-the-probe" {
		t.Errorf("probe's key: %q", raw)
	}
}

func TestSetupGivesNoDeclarationForADeviceWithAName(t *testing.T) {
	cloud := newFakeEWeLink(t, things)
	b := startWith(t, Config{}, fast, cloud.cloud(), relayID+"=greenhouse-heater-2")
	b.secret(appIDSecret, testAppID+"\n")
	b.secret(appSecretSecret, testAppSecret)
	b.device(relayID, "th_plug", relayOf("on")).report()
	b.eventually("the relay", func() bool { _, ok := b.reports()[relayID]; return ok })

	steps := b.setup(person{browse: signIn(t, "us")})
	if done := steps[len(steps)-1].Done; done.Error != "" {
		t.Fatalf("done: %+v", done)
	}
	if strings.Contains(said(steps), "home.Device(") {
		t.Errorf("said, of a device the home names already:\n%s", said(steps))
	}
}

func TestSetupSaysWhatWentWrong(t *testing.T) {
	for name, c := range map[string]struct {
		things string
		secret string
		browse func(t *testing.T) func(string) string
		want   string
	}{
		"wrong app secret": {
			things: things, secret: "not-the-secret",
			browse: func(t *testing.T) func(string) string { t.Helper(); return signIn(t, "eu") },
			want: "eWeLink refused to sign in: wrong sign (error 401): check the app's ID, secret and redirect URL, " +
				"and change them with oikade secret set ewelink-app-id and ewelink-app-secret",
		},
		"refused sign-in": {
			things: things, secret: testAppSecret,
			browse: func(*testing.T) func(string) string {
				return func(page string) string {
					u, _ := url.Parse(page)
					return url.Values{"state": {u.Query().Get("state")}, "error": {"access_denied"}}.Encode()
				}
			},
			want: "the sign-in sent back no code (access_denied)",
		},
		"another sign-in's answer": {
			things: things, secret: testAppSecret,
			browse: func(*testing.T) func(string) string {
				return func(string) string { return "code=" + testCode + "&region=eu&state=old" }
			},
			want: "another sign-in's answer",
		},
		"a region that isn't one": {
			things: things, secret: testAppSecret,
			browse: func(t *testing.T) func(string) string { t.Helper(); return signIn(t, "evil.example/") },
			want:   `the region "evil.example/"`,
		},
		"no devices": {
			things: `[]`, secret: testAppSecret,
			browse: func(t *testing.T) func(string) string { t.Helper(); return signIn(t, "eu") },
			want:   "your eWeLink account has no devices with a LAN key",
		},
		"only shared devices": {
			things: `[{"itemType":2,"itemData":{"deviceid":"1000aabbcc","name":"Shared"}}]`, secret: testAppSecret,
			browse: func(t *testing.T) func(string) string { t.Helper(); return signIn(t, "eu") },
			want:   "a device shared with you comes without its key",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cloud := newFakeEWeLink(t, c.things)
			b := startWith(t, Config{}, fast, cloud.cloud())
			b.secret(appIDSecret, testAppID)
			b.secret(appSecretSecret, c.secret)
			steps := b.setup(person{browse: c.browse(t)})
			done := steps[len(steps)-1].Done
			if !strings.Contains(done.Error, c.want) {
				t.Errorf("got %+v, want %q", done, c.want)
			}
			if strings.Contains(done.Error, testToken) || strings.Contains(done.Error, c.secret) {
				t.Errorf("the error gives away a secret: %s", done.Error)
			}
			if got := kept(steps); len(got) != 0 {
				t.Errorf("kept %v", got)
			}
		})
	}
}
