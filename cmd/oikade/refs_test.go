package main

import (
	"strings"
	"testing"
)

func TestACommandTakesARefOrABareName(t *testing.T) {
	for arg, want := range map[string]string{"overkiz": "overkiz", "plugin/overkiz": "overkiz"} {
		if got, err := pluginName(arg); err != nil || got != want {
			t.Errorf("pluginName(%s) = %q, %v", arg, got, err)
		}
	}
	for _, arg := range []string{"flow/morning", "plugin/overkiz/x", "plugin/Overkiz"} {
		if _, err := pluginName(arg); err == nil {
			t.Errorf("%s named a plugin", arg)
		}
	}
	_, err := pluginName("flow/morning")
	if err == nil || !strings.Contains(err.Error(), "give one as plugin/NAME") {
		t.Errorf("err = %v", err)
	}
	if d, c, err := capabilityOf("hardware/kasa/desk1/on"); err != nil || d != "kasa/desk1" || c != "on" {
		t.Errorf("capabilityOf = %q, %q, %v", d, c, err)
	}
	if _, _, err := capabilityOf("hardware/kasa/desk1"); err == nil {
		t.Error("a device named a capability")
	}
}

func TestSetWithoutAValueSaysHowToUseIt(t *testing.T) {
	err := runSet(t.Context(), []string{"hardware/kasa/desk1/on"})
	if err == nil || !strings.HasPrefix(err.Error(), "oikade set takes a capability") {
		t.Errorf("err = %v", err)
	}
	// A device and a capability by name aren't a ref.
	err = runSet(t.Context(), []string{"desk-plug", "on"})
	if err == nil || !strings.Contains(err.Error(), "give one as hardware/PLUGIN/ID/CAPABILITY") {
		t.Errorf("err = %v", err)
	}
}
