package ident

import (
	"slices"
	"strings"
	"testing"
)

func TestARefReadsBackAsItsParts(t *testing.T) {
	for _, c := range []struct {
		ref   Ref
		kind  Kind
		parts []string
	}{
		{NewRef(Device, "desk-plug"), Device, []string{"desk-plug"}},
		{CapabilityRef("desk-plug", "on"), Device, []string{"desk-plug", "on"}},
		{CaseRef("desk-plug", "on", "night"), Case, []string{"desk-plug", "on", "night"}},
		{NewRef(Flow, "morning"), Flow, []string{"morning"}},
		{NewRef(Room, "living-room"), Room, []string{"living-room"}},
		{NewRef(Hardware, "overkiz", "io://1234/5"), Hardware, []string{"overkiz", "io://1234/5"}},
		{NewRef(Matter), Matter, []string{}},
		{NewRef(Device), Device, []string{}},
		{NewRef(Command), Command, []string{}},
	} {
		kind, parts, err := ParseRef(string(c.ref))
		if err != nil || kind != c.kind || !slices.Equal(parts, c.parts) {
			t.Errorf("ParseRef(%s) = %s, %q, %v", c.ref, kind, parts, err)
		}
	}
	if got := NewRef(Hardware, "overkiz", "io://1234/5"); got != "hardware/overkiz/io:%2F%2F1234%2F5" {
		t.Errorf("an ID that isn't a name is %s", got)
	}
	if got := CaseRef("desk-plug", "on", "night"); got != "case/desk-plug/on/night" {
		t.Errorf("a case is %s", got)
	}
}

func TestABadRefSaysWhatOneLooksLike(t *testing.T) {
	for ref, want := range map[string]string{
		"desk-plug":                `one starts with its kind, one of device, hardware`,
		"rule/desk-plug":           `one starts with its kind`,
		"case/desk-plug/on":        `a case's is case/DEVICE/CAPABILITY/CASE`,
		"flow/a/b":                 `a flow's is flow/FLOW`,
		"matter/x":                 `a matter's is matter`,
		"device/a/b/c":             `a device's is device, device/DEVICE, device/DEVICE/CAPABILITY`,
		"flow/Good-Morning":        `"Good-Morning" isn't a flow name`,
		"case/desk-plug/On/night":  `"On" isn't a capability`,
		"flow/":                    `"" isn't a part of one`,
		"case/desk-plug/on/night/": `"" isn't a part of one`,
		"flow/%zz":                 `"%zz" isn't a part of one`,
	} {
		_, _, err := ParseRef(ref)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRef(%q) = %v, want an error with %s", ref, err, want)
		}
	}
}

func TestADotIDKeepsItsSegment(t *testing.T) {
	for _, id := range []string{".", ".."} {
		r := HardwareRef("relays", id)
		if strings.HasSuffix(r.String(), "/"+id) {
			t.Errorf("HardwareRef(%q) = %s, a dot segment", id, r)
		}
		if _, parts, err := ParseRef(r.String()); err != nil || parts[1] != id {
			t.Errorf("ParseRef(%s) = %q %v", r, parts, err)
		}
	}
}

func TestADeviceIsItsHardwaresRef(t *testing.T) {
	key := HardwareKey("overkiz", "io://1234/5")
	for ref, want := range map[Ref]string{
		DeviceRef(key):                    "hardware/overkiz/io:%2F%2F1234%2F5",
		CapabilityRef(key, "position"):    "hardware/overkiz/io:%2F%2F1234%2F5/position",
		CaseRef(key, "position", "dusk"):  "case/overkiz/io:%2F%2F1234%2F5/position/dusk",
		DeviceRef("porch-light"):          "device/porch-light",
		CaseRef("porch-light", "on", "a"): "case/porch-light/on/a",
	} {
		if ref.String() != want {
			t.Errorf("got %s, want %s", ref, want)
		}
		kind, parts, err := ParseRef(ref.String())
		if err != nil {
			t.Fatal(err)
		}
		got, rest, ok := DeviceOf(kind, parts)
		if wantKey := strings.Contains(want, "overkiz"); !ok || (wantKey && got != key) || (!wantKey && got != "porch-light") {
			t.Errorf("DeviceOf(%s) = %q, %q, %v", ref, got, rest, ok)
		}
	}
	if _, _, ok := DeviceOf(Flow, []string{"morning"}); ok {
		t.Error("a flow names a device")
	}
	// A plugin may be named as a kind is, and its case keeps it.
	if got := CaseRef(HardwareKey("device", "x"), "on", "night"); got != "case/device/x/on/night" {
		t.Errorf("got %s", got)
	}
}
