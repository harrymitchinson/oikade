package v1

import (
	"strings"
	"testing"
)

func TestSetupStepValidate(t *testing.T) {
	ok := func(s SetupStep) SetupStep { s.Session, s.Seq = "s", 1; return s }
	for _, s := range []SetupStep{
		ok(SetupStep{Say: "hello"}),
		ok(SetupStep{Ask: &SetupAsk{Prompt: "App ID"}}),
		ok(SetupStep{Ask: &SetupAsk{Prompt: "App secret", Secret: true}}),
		ok(SetupStep{Open: &SetupOpen{URL: "https://example.com/sign-in", Redirect: "http://127.0.0.1:8765/callback"}}),
		ok(SetupStep{Open: &SetupOpen{URL: "https://example.com/", Redirect: "http://localhost:8765/"}}),
		ok(SetupStep{Keep: &SetupKeep{Name: "app-secret", Value: ""}}),
		ok(SetupStep{Done: &SetupDone{}}),
		ok(SetupStep{Done: &SetupDone{Error: "refused"}}),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for want, s := range map[string]SetupStep{
		"no session":          {Seq: 1, Say: "hi"},
		"seq 0":               {Session: "s", Say: "hi"},
		"0 of say":            ok(SetupStep{}),
		"2 of say":            ok(SetupStep{Say: "hi", Done: &SetupDone{}}),
		"no prompt":           ok(SetupStep{Ask: &SetupAsk{}}),
		"isn't an https":      ok(SetupStep{Open: &SetupOpen{URL: "file:///etc/passwd", Redirect: "http://127.0.0.1:1/"}}),
		"isn't an https URL":  ok(SetupStep{Open: &SetupOpen{URL: "http://a.b/", Redirect: "http://127.0.0.1:1/"}}),
		"isn't on 127.0.0.1":  ok(SetupStep{Open: &SetupOpen{URL: "https://a.b/", Redirect: "http://example.com:80/cb"}}),
		"isn't http://":       ok(SetupStep{Open: &SetupOpen{URL: "https://a.b/", Redirect: "https://127.0.0.1:1/cb"}}),
		"no valid port":       ok(SetupStep{Open: &SetupOpen{URL: "https://a.b/", Redirect: "http://127.0.0.1:99999/cb"}}),
		"isn't a secret's":    ok(SetupStep{Keep: &SetupKeep{Name: "../escape"}}),
		"a result and an err": ok(SetupStep{Done: &SetupDone{Result: "ok", Error: "no"}}),
	} {
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: got %v, want an error with %q", s, err, want)
		}
	}
}

func TestRedirectAddress(t *testing.T) {
	for redirect, want := range map[string]string{
		"http://127.0.0.1:8765/callback": "127.0.0.1:8765",
		"http://localhost:9000/":         "127.0.0.1:9000",
		"http://LocalHost:9000/cb":       "127.0.0.1:9000",
	} {
		if got, err := RedirectAddress(redirect); err != nil || got != want {
			t.Errorf("%s: got %q, %v, want %s", redirect, got, err, want)
		}
	}
	for _, redirect := range []string{
		"http://127.0.0.1/callback", "http://0.0.0.0:80/", "http://[::1]:80/", "http://127.0.0.1:80/?a=b",
		"http://u@127.0.0.1:80/", "127.0.0.1:80",
	} {
		if _, err := RedirectAddress(redirect); err == nil {
			t.Errorf("%s: got no error", redirect)
		}
	}
}

func TestNeedsReply(t *testing.T) {
	for s, want := range map[*SetupStep]bool{
		{Say: "hi"}:                   false,
		{Done: &SetupDone{}}:          false,
		{Ask: &SetupAsk{Prompt: "x"}}: true,
		{Open: &SetupOpen{}}:          true,
		{Keep: &SetupKeep{Name: "x"}}: true,
	} {
		if got := s.NeedsReply(); got != want {
			t.Errorf("%+v: got %v, want %v", s, got, want)
		}
	}
}
