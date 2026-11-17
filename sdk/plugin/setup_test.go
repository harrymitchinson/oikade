package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

// hub sends setup requests as Oikade does, and reads the steps that come back.
type hub struct {
	t     *testing.T
	ctx   context.Context
	c     *mqtt.Client
	steps chan contractv1.SetupStep
}

func newHub(ctx context.Context, t *testing.T, url string) *hub {
	t.Helper()
	c, err := mqtt.Connect(ctx, url, fmt.Sprintf("hub-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	h := &hub{t: t, ctx: ctx, c: c, steps: make(chan contractv1.SetupStep, 16)}
	if _, err := c.Subscribe(ctx, root.SetupOut(), func(m mqtt.Message) {
		var s contractv1.SetupStep
		if err := json.Unmarshal(m.Payload, &s); err != nil || m.Retained {
			t.Errorf("plugin sent %s (retained %v): %v", m.Payload, m.Retained, err)
		}
		h.steps <- s
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *hub) send(r contractv1.SetupRequest) {
	h.t.Helper()
	payload, _ := json.Marshal(r)
	if err := h.c.Publish(h.ctx, root.SetupIn(), payload, false); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hub) next() contractv1.SetupStep {
	h.t.Helper()
	select {
	case s := <-h.steps:
		if err := s.Validate(); err != nil {
			h.t.Fatalf("plugin sent an invalid step %+v: %v", s, err)
		}
		return s
	case <-h.ctx.Done():
		h.t.Fatal("timed out waiting for a setup step")
		return contractv1.SetupStep{}
	}
}

func (h *hub) quiet() {
	h.t.Helper()
	select {
	case s := <-h.steps:
		h.t.Fatalf("plugin sent %+v", s)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSetupDialogue(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watchLive(ctx, t, b.URL)
	h := newHub(ctx, t, b.URL)
	kept := make(chan string, 1)
	_, logs := start(ctx, t, b.URL, Options{OnSetup: func(ctx context.Context, _ *Plugin, s *Session) (string, error) {
		if err := s.Say(ctx, "Make an app first"); err != nil {
			return "", err
		}
		if err := s.Say(ctx, ""); err != nil { // says nothing, and takes no number
			return "", err
		}
		if _, err := s.Ask(ctx, ""); err == nil { // invalid, and takes no number
			return "", errors.New("asked with no prompt")
		}
		id, err := s.Ask(ctx, "App ID")
		if err != nil {
			return "", err
		}
		secret, err := s.AskSecret(ctx, "App secret")
		if err != nil {
			return "", err
		}
		q, err := s.Open(ctx, "https://example.com/oauth", "http://127.0.0.1:8765/callback")
		if err != nil {
			return "", err
		}
		if err := s.KeepSecret(ctx, "app-secret", secret); err != nil {
			return "", err
		}
		kept <- secret
		return "signed in as " + id + " with code " + q.Get("code"), nil
	}})
	w.expect(root.Status() + `={"state":"online","contract":1,"setup":true}`)

	h.send(contractv1.SetupRequest{Session: "s", Start: true})
	if s := h.next(); s.Seq != 1 || s.Say != "Make an app first" {
		t.Fatalf("got %+v, want the say", s)
	}
	if s := h.next(); s.Seq != 2 || s.Ask == nil || s.Ask.Prompt != "App ID" || s.Ask.Secret {
		t.Fatalf("got %+v, want the ask", s)
	}
	// A reply to a step that isn't waiting is dropped.
	h.send(contractv1.SetupRequest{Session: "s", Reply: 7, Value: "nobody"})
	h.send(contractv1.SetupRequest{Session: "other", Reply: 2, Value: "nobody"})
	h.send(contractv1.SetupRequest{Session: "s", Reply: 2, Value: "app-1"})
	if s := h.next(); s.Seq != 3 || s.Ask == nil || !s.Ask.Secret {
		t.Fatalf("got %+v, want the secret ask", s)
	}
	h.send(contractv1.SetupRequest{Session: "s", Reply: 3, Value: "hunter2"})
	if s := h.next(); s.Seq != 4 || s.Open == nil || s.Open.Redirect != "http://127.0.0.1:8765/callback" {
		t.Fatalf("got %+v, want the open", s)
	}
	h.send(contractv1.SetupRequest{Session: "s", Reply: 4, Value: "code=xyz&state=1"})
	if s := h.next(); s.Seq != 5 || s.Keep == nil || s.Keep.Name != "app-secret" || s.Keep.Value != "hunter2" {
		t.Fatalf("got %+v, want the keep", s)
	}
	h.send(contractv1.SetupRequest{Session: "s", Reply: 5})
	if got := <-kept; got != "hunter2" {
		t.Fatalf("kept %q", got)
	}
	if s := h.next(); s.Seq != 6 || s.Done == nil || s.Done.Result != "signed in as app-1 with code xyz" {
		t.Fatalf("got %+v, want done", s)
	}
	h.quiet()
	if strings.Contains(logs.String(), "hunter2") {
		t.Fatalf("logged a secret:\n%s", logs)
	}
}

func TestSetupFailsWithTheError(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	h := newHub(ctx, t, b.URL)
	start(ctx, t, b.URL, Options{OnSetup: func(ctx context.Context, _ *Plugin, s *Session) (string, error) {
		if err := s.KeepSecret(ctx, "token", "t"); err != nil {
			return "", fmt.Errorf("could not keep the token: %w", err)
		}
		return "ok", nil
	}})
	h.send(contractv1.SetupRequest{Session: "s", Start: true})
	if s := h.next(); s.Keep == nil {
		t.Fatalf("got %+v, want the keep", s)
	}
	h.send(contractv1.SetupRequest{Session: "s", Reply: 1, Error: "the secrets directory is read-only"})
	if s := h.next(); s.Done == nil || s.Done.Error != "could not keep the token: the secrets directory is read-only" {
		t.Fatalf("got %+v, want done with the error", s)
	}
}

func TestAPluginWithNoSetupSaysSo(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	h := newHub(ctx, t, b.URL)
	p, _ := start(ctx, t, b.URL, Options{})
	h.send(contractv1.SetupRequest{Session: "s", Start: true})
	if s := h.next(); s.Done == nil || s.Done.Error != "this plugin has no setup" {
		t.Fatalf("got %+v, want done saying there's no setup", s)
	}
	if err := p.SetNeedsSetup(ctx, true); err == nil {
		t.Fatal("a plugin with no setup said it needs one")
	}
	if err := p.SetNeedsSetup(ctx, false); err != nil {
		t.Fatal(err)
	}
}

func TestEndingSetupEndsTheSession(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	h := newHub(ctx, t, b.URL)
	ended := make(chan error, 2)
	start(ctx, t, b.URL, Options{OnSetup: func(ctx context.Context, _ *Plugin, s *Session) (string, error) {
		_, err := s.Ask(ctx, "Code")
		ended <- err
		return "", err
	}})
	h.send(contractv1.SetupRequest{Session: "s1", Start: true})
	h.next()
	// A new session ends the one before it.
	h.send(contractv1.SetupRequest{Session: "s2", Start: true})
	if err := <-ended; !errors.Is(err, ErrSetupEnded) {
		t.Fatalf("the first session's ask: %v, want ErrSetupEnded", err)
	}
	if s := h.next(); s.Session != "s2" || s.Ask == nil {
		t.Fatalf("got %+v, want the second session's ask", s)
	}
	h.send(contractv1.SetupRequest{Session: "s1", End: true}) // not the running one
	h.send(contractv1.SetupRequest{Session: "s2", End: true})
	if err := <-ended; !errors.Is(err, ErrSetupEnded) {
		t.Fatalf("the second session's ask: %v, want ErrSetupEnded", err)
	}
	// An ended session says nothing more: the hub ended it.
	h.quiet()
}

func TestNeedsSetupGoesWithTheDevices(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watchLive(ctx, t, b.URL)
	p, _ := start(ctx, t, b.URL, Options{OnSetup: func(context.Context, *Plugin, *Session) (string, error) {
		return "", nil
	}})
	w.expect(root.Status() + `={"state":"online","contract":1,"setup":true}`)
	// Before the devices are set it waits for them.
	if err := p.SetNeedsSetup(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDevices(ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	w.expect(root.Devices() + `={"devices":[],"undeclared":0,"needsSetup":true}`)
	if err := p.SetNeedsSetup(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNeedsSetup(ctx, false); err != nil {
		t.Fatal(err)
	}
	w.expect(root.Devices() + `={"devices":[],"undeclared":0}`)
	w.quiet(b.URL)
}

// A session that ends while a step waits to be published ends the step with ErrSetupEnded, not
// the publish's own context error.
func TestASessionEndingDuringAPublishEndsTheStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Plugin{pub: make(chan struct{}, 1)}
		p.pub <- struct{}{} // another publish holds the lock
		ctx, end := context.WithCancel(t.Context())
		s := &Session{p: p, name: "s1", ctx: ctx, waiting: map[int]chan contractv1.SetupRequest{}}
		got := make(chan error, 1)
		go func() {
			_, err := s.Ask(ctx, "Code")
			got <- err
		}()
		synctest.Wait() // the Ask waits for the lock
		end()
		if err := <-got; !errors.Is(err, ErrSetupEnded) {
			t.Fatalf("the ask: %v, want ErrSetupEnded", err)
		}
	})
}
