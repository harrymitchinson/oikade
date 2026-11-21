package plugin

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/mqtt"
)

func withSetup() map[string]string {
	return map[string]string{
		"status":  `{"state":"online","contract":1,"setup":true}`,
		"devices": `{"devices":[],"undeclared":0,"needsSetup":true}`,
	}
}

// requests collects what the decoder sends on the setup topic.
func requests(t *testing.T, bus *mqtt.Memory) <-chan contract.SetupRequest {
	t.Helper()
	got := make(chan contract.SetupRequest, 16)
	if _, err := bus.Subscribe(t.Context(), root+"/_setup/in", func(m mqtt.Message) {
		var r contract.SetupRequest
		if err := json.Unmarshal(m.Payload, &r); err != nil {
			t.Errorf("decoder sent %s: %v", m.Payload, err)
		}
		got <- r
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func step(t *testing.T, steps <-chan contract.SetupStep) (contract.SetupStep, bool) {
	t.Helper()
	select {
	case s, ok := <-steps:
		return s, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no setup step")
		return contract.SetupStep{}, false
	}
}

func say(t *testing.T, bus *mqtt.Memory, payload string) {
	t.Helper()
	if err := bus.Publish(t.Context(), root+"/_setup/out", []byte(payload), false); err != nil {
		t.Fatal(err)
	}
}

func TestSetupFlags(t *testing.T) {
	i, _, _ := setup(t, online())
	if has, needs := i.Setup(); has || needs {
		t.Fatalf("a plugin with no setup: got %v, %v", has, needs)
	}
	if _, err := i.StartSetup(t.Context(), "s"); !errors.Is(err, ErrNoSetup) {
		t.Fatalf("StartSetup on a plugin with no setup: %v, want ErrNoSetup", err)
	}
	i, bus, _ := setup(t, withSetup())
	if has, needs := i.Setup(); !has || !needs {
		t.Fatalf("got %v, %v, want a setup that's needed", has, needs)
	}
	publish(t, bus, root+"/status", `{"state":"offline","contract":1,"setup":true}`)
	if has, needs := i.Setup(); has || needs {
		t.Fatalf("an offline plugin: got %v, %v", has, needs)
	}
}

func TestSetupSession(t *testing.T) {
	i, bus, _ := setup(t, withSetup())
	sent := requests(t, bus)
	ctx := t.Context()
	steps, err := i.StartSetup(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if r := <-sent; r.Session != "s1" || !r.Start {
		t.Fatalf("sent %+v, want a start", r)
	}
	if _, err := i.StartSetup(ctx, "s1"); err == nil {
		t.Fatal("started s1 twice")
	}

	say(t, bus, `{"session":"other","seq":1,"say":"not ours"}`)
	say(t, bus, `{"session":"s1","seq":1,"ask":{"prompt":"App ID"}}`)
	if s, _ := step(t, steps); s.Ask == nil || s.Ask.Prompt != "App ID" {
		t.Fatalf("got %+v, want the ask", s)
	}
	if err := i.ReplySetup(ctx, "s1", 1, "abc", ""); err != nil {
		t.Fatal(err)
	}
	if r := <-sent; r.Session != "s1" || r.Reply != 1 || r.Value != "abc" {
		t.Fatalf("sent %+v, want the reply", r)
	}
	say(t, bus, `{"session":"s1","seq":2,"done":{"result":"set up"}}`)
	if s, _ := step(t, steps); s.Done == nil || s.Done.Result != "set up" {
		t.Fatalf("got %+v, want done", s)
	}
	if _, ok := step(t, steps); ok {
		t.Fatal("steps go on after done")
	}
	if r := i.Stats().Rejected; len(r) > 0 {
		t.Fatalf("rejected %v", r)
	}
}

func TestSetupEndsWhenThePluginBreaksIt(t *testing.T) {
	i, bus, _ := setup(t, withSetup())
	sent := requests(t, bus)
	steps, err := i.StartSetup(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	<-sent
	say(t, bus, `{"session":"s1","seq":1,"say":"a","done":{}}`)
	s, _ := step(t, steps)
	if s.Seq != 0 || s.Done == nil || s.Done.Error == "" {
		t.Fatalf("got %+v, want the decoder's own done with the reason", s)
	}
	if _, ok := step(t, steps); ok {
		t.Fatal("steps go on after done")
	}
	if n := i.Stats().Rejected[ReasonMalformed]; n != 1 {
		t.Fatalf("rejected %d malformed, want 1", n)
	}
	// The plugin is told, so it doesn't wait for a reply.
	if r := <-sent; r.Session != "s1" || !r.End {
		t.Fatalf("sent %+v, want an end", r)
	}
}

func TestSetupEndsWhenThePluginNoLongerHasOne(t *testing.T) {
	i, bus, _ := setup(t, withSetup())
	steps, err := i.StartSetup(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	publish(t, bus, root+"/status", `{"state":"online","contract":1}`)
	if s, _ := step(t, steps); s.Done == nil || s.Done.Error != "the plugin no longer has a setup" {
		t.Fatalf("got %+v, want done because the setup went", s)
	}
}

func TestSetupEndsWhenThePluginGoesOffline(t *testing.T) {
	i, bus, _ := setup(t, withSetup())
	steps, err := i.StartSetup(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	publish(t, bus, root+"/status", `{"state":"offline","contract":1}`)
	if s, _ := step(t, steps); s.Done == nil || s.Done.Error != "the plugin went offline" {
		t.Fatalf("got %+v, want done because the plugin went", s)
	}
}

func TestEndAndRestartSetup(t *testing.T) {
	i, bus, _ := setup(t, withSetup())
	sent := requests(t, bus)
	ctx := t.Context()
	first, err := i.StartSetup(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	<-sent
	// A second session ends the first, as the plugin does.
	second, err := i.StartSetup(ctx, "s2")
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := step(t, first); s.Done == nil || s.Done.Error == "" {
		t.Fatalf("got %+v, want the first ended", s)
	}
	<-sent
	if err := i.EndSetup(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	if r := <-sent; r.Session != "s2" || !r.End {
		t.Fatalf("sent %+v, want an end", r)
	}
	if _, ok := step(t, second); ok {
		t.Fatal("an ended session has a step")
	}
	if err := i.EndSetup(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-sent:
		t.Fatalf("ending an ended session sent %+v", r)
	default:
	}
}
