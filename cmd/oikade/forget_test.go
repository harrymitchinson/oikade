package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/testkit"
)

// What forget and remove say they did, plain and styled.
func TestForgetAndRemoveSayWhatTheyDid(t *testing.T) {
	porch := api.Forgotten{
		Device: "porch-light", Ref: "device/porch-light", Plugin: "ewelink-lan",
		Ended: []string{"case/porch-light/on/dusk", "flow/away"}, OffMatter: true,
	}
	forgets := []struct {
		name      string
		forgotten []api.Forgotten
		plugin    string
	}{
		{"one device, held and on the bridge, of a plugin your home imports", []api.Forgotten{porch}, ""},
		{"a device with neither, of a driver that isn't a plugin", []api.Forgotten{
			{Device: "shed-lamp", Ref: "device/shed-lamp", ComesBack: true},
		}, ""},
		{"a plugin's devices, which your home doesn't import", []api.Forgotten{
			{Device: "awning", Ref: "device/awning", Plugin: "relays", OffMatter: true, Unimported: true},
			{Device: "boiler", Ref: "device/boiler", Plugin: "relays", Ended: []string{"flow/frost"}, Unimported: true},
		}, "relays"},
		{"a plugin's devices, which your home imports", []api.Forgotten{
			{Device: "awning", Ref: "device/awning", Plugin: "relays"},
			{Device: "boiler", Ref: "device/boiler", Plugin: "relays"},
		}, "relays"},
		{"devices of two plugins", []api.Forgotten{
			porch,
			{Device: "boiler", Ref: "device/boiler", Plugin: "relays", Unimported: true},
		}, ""},
	}
	removes := []struct {
		ref       string
		cancelled bool
	}{{"client/old-laptop", false}, {"client/phone", true}}
	for name, printer := range printers {
		var b bytes.Buffer
		for _, f := range forgets {
			_, _ = fmt.Fprintf(&b, "--- forget: %s\n", f.name)
			printForgotten(printer(&b, 80), f.forgotten, f.plugin)
		}
		for _, r := range removes {
			_, _ = fmt.Fprintf(&b, "--- remove %s\n", r.ref)
			kind, parts, err := ident.ParseRef(r.ref)
			if err != nil {
				t.Fatal(err)
			}
			printer(&b, 80).Done("%s", removedMessage(named{ref: r.ref, kind: kind, parts: parts}, r.cancelled))
		}
		testkit.Golden(t, "forget-remove-"+name, b.Bytes())
	}
}

// forget takes devices and remove clients, and each, given the other's, names it. A secret is one of
// your values, in .env, which each names. Each is a command typed wrong, which exits 2 before
// anything reaches a host.
func TestForgetAndRemoveNameEachOther(t *testing.T) {
	var b bytes.Buffer
	for _, args := range []string{
		"forget", "forget secret/api-key", "forget client/old-laptop", "forget plugin/relays", "forget room/hall",
		"forget matter", "forget porch-light --plugin relays", "forget Porch",
		"remove", "remove porch-light", "remove device/porch-light", "remove plugin/relays", "remove client/Bad",
		"remove secret/api-key", "secret remove api-key", "client revoke old-laptop", "devices remove porch-light",
		"revoke", "rm", "add",
	} {
		_, errs, code := runOikade(t, cli.Plain, strings.Fields(args)...)
		_, _ = fmt.Fprintf(&b, "$ oikade %s\n%sexit %d\n\n", args, errs, code)
		if code != 2 {
			t.Errorf("oikade %s exits %d", args, code)
		}
	}
	testkit.Golden(t, "forget-remove-usage-plain", b.Bytes())
}

// The same thing twice is removed, or forgotten, once.
func TestRemoveAndForgetTakeEachOnce(t *testing.T) {
	refs, err := removable([]string{"client/old-laptop", "client/ci", "client/old-laptop"})
	if err != nil || len(refs) != 2 || refs[0].ref != "client/old-laptop" || refs[1].ref != "client/ci" {
		t.Errorf("got %+v, %v", refs, err)
	}
	if names, err := forgettable([]string{"porch-light", "device/porch-light"}); err != nil || len(names) != 1 {
		t.Errorf("got %v, %v", names, err)
	}
}

// remove --json says how it ended when it fails before removing anything, as when the host is out of
// reach.
func TestRemoveJSONSaysItFailed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out, _, code := runOikade(t, cli.Plain, "remove", "client/old-laptop", "--json", "--data-dir", t.TempDir())
	if code != 1 || !strings.HasPrefix(out, `{"ok":false,"error":`) || !strings.HasSuffix(out, `"refs":[]}`+"\n") {
		t.Errorf("exit %d: %s", code, out)
	}
}

// set takes a capability's ref, pair takes matter, and the forms they replaced name them, as set
// given a secret names .env. Each is a command typed wrong, which exits 2 before anything reaches a
// host.
func TestSetAndPairNameWhatTheyTake(t *testing.T) {
	var b bytes.Buffer
	for _, args := range []string{
		"set secret/api-key", "set desk-plug on true", "set device/desk-plug/on",
		"pair", "pair device/desk-plug", "pair matter --for 0s", "secret set api-key", "matter", "matter open",
		"open", "deploy --allow-safety-removal", "rollback --allow-safety-removal=true", "init --module example.com/home",
		"pair matter --for 1m", "pair matter --for 20m",
	} {
		_, errs, code := runOikade(t, cli.Plain, strings.Fields(args)...)
		_, _ = fmt.Fprintf(&b, "$ oikade %s\n%sexit %d\n\n", args, errs, code)
		if code != 2 {
			t.Errorf("oikade %s exits %d", args, code)
		}
	}
	testkit.Golden(t, "set-pair-usage-plain", b.Bytes())
}
