package hue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/mdns"
	"oikade.dev/internal/mqtt"
)

// setup runs the plugin's setup as the host would, answering each question with answer, and returns
// its steps.
func (b *bench) setup(answer func(prompt string) string) []contract.SetupStep {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	c, err := mqtt.Connect(ctx, b.env.MQTTURL, "hue-setup")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(context.Background()) }()
	got := make(chan contract.SetupStep, 64)
	if _, err := c.Subscribe(ctx, root.SetupOut(), func(m mqtt.Message) {
		var s contract.SetupStep
		if json.Unmarshal(m.Payload, &s) == nil {
			got <- s
		}
	}); err != nil {
		b.t.Fatal(err)
	}
	send := func(r contract.SetupRequest) {
		raw, _ := json.Marshal(r)
		if err := c.Publish(ctx, root.SetupIn(), raw, false); err != nil {
			b.t.Fatal(err)
		}
	}
	send(contract.SetupRequest{Session: "s", Start: true})
	var steps []contract.SetupStep
	for {
		select {
		case s := <-got:
			steps = append(steps, s)
			switch {
			case s.Done != nil:
				return steps
			case s.Ask != nil:
				if answer == nil {
					b.t.Fatalf("unexpected question %q", s.Ask.Prompt)
				}
				send(contract.SetupRequest{Session: "s", Reply: s.Seq, Value: answer(s.Ask.Prompt)})
			case s.Keep != nil:
				b.keepAs(s.Keep.Name, s.Keep.Value)
				send(contract.SetupRequest{Session: "s", Reply: s.Seq})
			}
		case <-ctx.Done():
			b.t.Fatalf("setup didn't finish: %+v", steps)
		}
	}
}

func said(steps []contract.SetupStep) string {
	var out []string
	for _, s := range steps {
		if s.Say != "" {
			out = append(out, s.Say)
		}
	}
	return strings.Join(out, "\n")
}

func keptNames(steps []contract.SetupStep) []string {
	var out []string
	for _, s := range steps {
		if s.Keep != nil {
			out = append(out, s.Keep.Name)
		}
	}
	return out
}

func done(t *testing.T, steps []contract.SetupStep) contract.SetupDone {
	t.Helper()
	last := steps[len(steps)-1]
	if last.Done == nil {
		t.Fatalf("setup ended with %+v", last)
	}
	return *last.Done
}

func (b *bench) waitNeedsSetup(want bool) {
	b.t.Helper()
	b.eventually(fmt.Sprintf("needs setup %v", want), func() bool { return b.devices().NeedsSetup == want })
}

func TestSetupWaitsForTheButtonKeepsTheKeyAndThePluginComesOnline(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: "none", declared: []string{hallLight}})
	b.waitNeedsSetup(true)
	if s := b.status(); s.Error != "" || !s.Setup {
		t.Errorf("status %+v, want a setup and no problem, as needing setup says it", s)
	}
	f.pressAfter(3)
	steps := b.setup(nil)
	if got, want := said(steps), "Looking for Hue bridges over mDNS\nFound Hue Bridge Pro "+bridgeOne+"\nPress the link button on the bridge"; got != want {
		t.Errorf("said %q, want %q", got, want)
	}
	if d := done(t, steps); d.Result != "hue is set up: 4 devices" || d.Error != "" {
		t.Errorf("done %+v", d)
	}
	if got := f.asked(); len(got) != 4 || got[3] != "oikade#hue" {
		t.Errorf("asked for a key %q, want 4 times as oikade#hue", got)
	}
	if got, want := b.kept(), keptSecret(bridgeOne, appKey); got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
	b.waitOnline()
	b.waitNeedsSetup(false)
}

func TestSetupAgainWithAKeyThatWorksKeepsNothing(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	steps := b.setup(nil)
	if got := keptNames(steps); len(got) != 0 {
		t.Errorf("kept %q, want nothing", got)
	}
	if d := done(t, steps); d.Result != "hue was set up already, and its key still works: 4 devices" {
		t.Errorf("done %+v", d)
	}
	if got := f.asked(); len(got) != 0 {
		t.Errorf("asked for a key %q", got)
	}
}

func TestSetupAfterTheKeyIsRefusedAsksForAnother(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: keptSecret(bridgeOne, "removed-in-the-app")})
	b.waitProblem("refused the plugin's key")
	b.waitNeedsSetup(true)
	steps := b.setup(nil)
	if !strings.Contains(said(steps), "The bridge refused the key setup kept before") {
		t.Errorf("said %q", said(steps))
	}
	if d := done(t, steps); d.Result != "hue is set up: 4 devices" {
		t.Errorf("done %+v", d)
	}
	b.waitOnline()
	b.waitNeedsSetup(false)
}

func TestSetupWithTwoBridgesAsksWhich(t *testing.T) {
	ca := newAuthority(t)
	one, two := newFakeBridgeFrom(t, bridgeOne, ca), newFakeBridgeFrom(t, bridgeTwo, ca)
	b := start(t, one, options{
		secret: "none",
		bridges: func(context.Context, time.Duration, string) ([]found, error) {
			return []found{{id: bridgeTwo, model: "BSB002", address: two.address()}, {id: bridgeOne, model: "BSB003", address: one.address()}}, nil
		},
		find: func(_ context.Context, id string, _ time.Duration) (string, error) {
			if id == bridgeTwo {
				return two.address(), nil
			}
			return "", mdns.ErrNotFound
		},
	})
	b.waitNeedsSetup(true)
	var asked []string
	steps := b.setup(func(prompt string) string {
		asked = append(asked, prompt)
		return " " + strings.ToLower(bridgeTwo) + "\n"
	})
	if want := []string{"Which bridge? Type its ID"}; strings.Join(asked, "|") != strings.Join(want, "|") {
		t.Errorf("asked %q, want %q", asked, want)
	}
	if got, want := said(steps), "Looking for Hue bridges over mDNS\nFound 2 Hue bridges: Hue Bridge "+bridgeTwo+", Hue Bridge Pro "+bridgeOne+
		"\nPress the link button on the bridge"; got != want {
		t.Errorf("said %q, want %q", got, want)
	}
	if got, want := b.kept(), keptSecret(bridgeTwo, appKey); got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
	if len(one.asked()) != 0 || len(two.asked()) != 1 {
		t.Errorf("asked the first bridge %q and the second %q, want the second once", one.asked(), two.asked())
	}
	b.waitOnline()
}

func TestSetupAsksBeforeTakingAnotherBridgeThanTheOneItKept(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{secret: keptSecret(bridgeTwo, appKey)})
	var asked int
	steps := b.setup(func(string) string {
		asked++
		return bridgeOne
	})
	if got, want := said(steps), "Looking for Hue bridges over mDNS\nThe Hue bridge "+bridgeTwo+", which setup kept before, didn't answer\n"+
		"Found Hue Bridge Pro "+bridgeOne+"\nPress the link button on the bridge"; got != want {
		t.Errorf("said %q, want %q", got, want)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want once", asked)
	}
	if got, want := b.kept(), keptSecret(bridgeOne, appKey); got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
}

func TestWithPort(t *testing.T) {
	for address, want := range map[string]string{
		"192.168.1.30":       "192.168.1.30:443",
		"192.168.1.30:8443":  "192.168.1.30:8443",
		"hue.local":          "hue.local:443",
		"fe80::1":            "[fe80::1]:443",
		"[fe80::1]":          "[fe80::1]:443",
		"[fe80::1]:443":      "[fe80::1]:443",
		"bridge.example:443": "bridge.example:443",
	} {
		if got := withPort(address); got != want {
			t.Errorf("withPort(%q) = %q, want %q", address, got, want)
		}
	}
}

func TestSetupAtAnAddressReadsTheBridgesIDFromItsCertificate(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{
		secret: "none",
		config: Config{Address: f.address()},
		bridges: func(context.Context, time.Duration, string) ([]found, error) {
			return nil, errors.New("mDNS mustn't be asked")
		},
	})
	steps := b.setup(nil)
	if !strings.HasPrefix(said(steps), "Reaching the Hue bridge at "+f.address()+"\nFound Hue Bridge Pro "+bridgeOne+"\n") {
		t.Errorf("said %q", said(steps))
	}
	if got, want := b.kept(), keptSecret(bridgeOne, appKey); got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
	b.waitOnline()
}

func TestSetupSaysWhatWentWrong(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	none := func(context.Context, time.Duration, string) ([]found, error) { return nil, nil }
	two := func(context.Context, time.Duration, string) ([]found, error) {
		return []found{{id: bridgeOne, address: f.address()}, {id: bridgeTwo, address: f.address()}}, nil
	}
	impatient := fast
	impatient.press = 30 * time.Millisecond
	for _, c := range []struct {
		name    string
		options options
		answer  string
		unpress int
		refusal string
		want    string
	}{
		{name: "no bridge", options: options{bridges: none}, want: "found no Hue bridge on the network over mDNS: check it's on and on the host's network, or set hue.Config's Address"},
		{name: "another ID", options: options{bridges: two}, answer: "C42996", want: `"C42996" isn't one of the bridges' IDs: run setup again`},
		{name: "no press", options: options{timing: impatient}, unpress: 1 << 20, want: "the link button on Hue Bridge Pro " + bridgeOne + " wasn't pressed within 30ms: run setup again, and press it then"},
		{name: "a refusal", refusal: "invalid value, oikade#hue, for parameter, devicetype",
			want: "Hue Bridge Pro " + bridgeOne + " refused to give a key: invalid value, oikade#hue, for parameter, devicetype"},
		{name: "another authority", options: options{config: Config{Address: f.address(), roots: newAuthority(t).pool}}, want: "the Hue bridge at " + f.address() + " has a certificate that for " + bridgeOne + " isn't signed by Philips Hue"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.options.secret = "none"
			b := start(t, f, c.options)
			f.pressAfter(c.unpress)
			f.refuseLinks(c.refusal)
			d := done(t, b.setup(func(string) string { return c.answer }))
			if !strings.Contains(d.Error, c.want) {
				t.Errorf("done %+v, want the error %q", d, c.want)
			}
			if b.kept() != "" {
				t.Errorf("kept %s", b.kept())
			}
		})
	}
}
