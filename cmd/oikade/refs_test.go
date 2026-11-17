package main

import (
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/ident"
)

func TestACommandTakesARefOrABareName(t *testing.T) {
	for arg, want := range map[string]string{"desk-plug": "desk-plug", "device/desk-plug": "desk-plug"} {
		if got, err := nameOf(ident.Device, arg); err != nil || got != want {
			t.Errorf("nameOf(%s) = %q, %v", arg, got, err)
		}
	}
	for _, arg := range []string{"flow/morning", "device/desk-plug/on", "device/Desk"} {
		if _, err := nameOf(ident.Device, arg); err == nil {
			t.Errorf("%s named a device", arg)
		}
	}
	_, err := nameOf(ident.Device, "flow/morning")
	if err == nil || !strings.Contains(err.Error(), "such as device/NAME") {
		t.Errorf("err = %v", err)
	}
	if d, c, err := capabilityOf("device/desk-plug/on"); err != nil || d != "desk-plug" || c != "on" {
		t.Errorf("capabilityOf = %q, %q, %v", d, c, err)
	}
	if _, _, err := capabilityOf("device/desk-plug"); err == nil {
		t.Error("a device named a capability")
	}
}
