package overkiz

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

func TestADeviceURLMakesAnIDWithoutItsHub(t *testing.T) {
	for _, tc := range []struct {
		url, id, base, sub string
		ok                 bool
	}{
		{url: "io://1234-5678-9012/11111111", id: "io.11111111", base: "io://1234-5678-9012/11111111", ok: true},
		{url: "io://1234-5678-9012/11111111#2", id: "io.11111111", base: "io://1234-5678-9012/11111111", sub: "2", ok: true},
		// #1 is the device itself.
		{url: "io://1234-5678-9012/11111111#1", id: "io.11111111", base: "io://1234-5678-9012/11111111", ok: true},
		// Characters an ID can't have become '-'.
		{url: "internal://1234-5678-9012/pod/0", id: "internal.pod-0", base: "internal://1234-5678-9012/pod/0", ok: true},
		{url: "zigbee://1234-5678-9012/ab:cd", id: "zigbee.ab-cd", base: "zigbee://1234-5678-9012/ab:cd", ok: true},
		{url: "io:/1234/1"},
		{url: "io://1234-5678-9012"},
		{url: "://1234/1"},
	} {
		u, ok := parseURL(tc.url)
		if ok != tc.ok {
			t.Errorf("%s: parsed %v, want %v", tc.url, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		id, valid := u.id()
		if id != tc.id || !valid || u.base() != tc.base || u.sub != tc.sub {
			t.Errorf("%s: id %q (valid %v), base %q, sub %q", tc.url, id, valid, u.base(), u.sub)
		}
	}
	// An address too long for an ID isn't one.
	long, _ := parseURL("io://1234/" + string(make([]byte, 70)))
	if _, ok := long.id(); ok {
		t.Error("a 70-character address made an ID")
	}
}

func TestAClosureIsAPosition(t *testing.T) {
	read := func(v any) contractv1.State { return closure.values(states{{"", "core:ClosureState"}: v}) }
	if got := read(40.0); got[contractv1.Position] != 40.0 {
		t.Errorf("closure 40: %v", got)
	}
	if got := read(101.0); len(got) != 0 {
		t.Errorf("closure 101: %v", got)
	}
	if _, got, err := closure.write(contractv1.Position, 42.6, nil); err != nil || len(got) != 1 || got[0].Name != "setClosure" || got[0].Parameters[0] != 43 {
		t.Errorf("writing 42.6: %+v, %v", got, err)
	}
	// Anything else is refused rather than sent, so it can't open a shutter as 0 would.
	for _, v := range []any{true, 150.0, -1.0, "50"} {
		if _, got, err := closure.write(contractv1.Position, v, nil); err == nil {
			t.Errorf("writing %v: %+v", v, got)
		}
	}
}

func TestATowelRadiatorsSetpointFollowsItsMode(t *testing.T) {
	radiator := func(mode string) states {
		return states{
			{"", "core:OperatingModeState"}: mode, {"", "core:TargetTemperatureState"}: 21.0,
			{"", "io:EffectiveTemperatureSetpointState"}: 18.5, {towelSensor, "core:TemperatureState"}: 19.25,
		}
	}
	for _, tc := range []struct {
		mode     string
		want     contractv1.State
		on, warm string // the commands for on true, and a setpoint of 22
	}{
		{modeStandby, contractv1.State{"on": false, "setpoint": 21.0, "temperature": 19.25}, "setTowelDryerOperatingMode external", "setTargetTemperature 22"},
		{modeExternal, contractv1.State{"on": true, "setpoint": 21.0, "temperature": 19.25}, "", "setTargetTemperature 22"},
		{modeInternal, contractv1.State{"on": true, "setpoint": 18.5, "temperature": 19.25}, "", "setDerogatedTargetTemperature 22"},
		{modeAuto, contractv1.State{"on": true, "setpoint": 18.5, "temperature": 19.25}, "", "setDerogatedTargetTemperature 22"},
		// A mode it doesn't know says nothing about on.
		{"boost", contractv1.State{"setpoint": 21.0, "temperature": 19.25}, "setTowelDryerOperatingMode external", "setTargetTemperature 22"},
	} {
		s := radiator(tc.mode)
		if got := towelDryer.values(s); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: values %v, want %v", tc.mode, got, tc.want)
		}
		if got := written(t, s, contractv1.On, true); got != tc.on {
			t.Errorf("%s: on true sends %q, want %q", tc.mode, got, tc.on)
		}
		if got := written(t, s, contractv1.Setpoint, 22.0); got != tc.warm {
			t.Errorf("%s: setpoint 22 sends %q, want %q", tc.mode, got, tc.warm)
		}
		if got := written(t, s, contractv1.On, false); got != "setTowelDryerOperatingMode standby" {
			t.Errorf("%s: on false sends %q", tc.mode, got)
		}
	}
	for _, bad := range []struct {
		capability string
		v          any
	}{{contractv1.Setpoint, 60.0}, {contractv1.On, 1.0}, {contractv1.Temperature, 20.0}} {
		if _, got, err := towelDryer.write(bad.capability, bad.v, radiator(modeExternal)); err == nil {
			t.Errorf("%s %v: sent %+v", bad.capability, bad.v, got)
		}
	}
}

// written is the commands a write sends, as "name parameters".
func written(t *testing.T, s states, capability string, v any) string {
	t.Helper()
	sub, commands, err := towelDryer.write(capability, v, s)
	if err != nil || sub != "" {
		t.Fatalf("%s %v: sub %q, %v", capability, v, sub, err)
	}
	var out []string
	for _, c := range commands {
		out = append(out, strings.TrimSpace(c.Name+" "+strings.Trim(fmt.Sprint(c.Parameters), "[]")))
	}
	return strings.Join(out, "; ")
}

func TestErrorsAreClassifiedByWhatToDoNext(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want failure
	}{
		{errors.New("dial tcp: connection refused"), unreachable},
		{&apiError{Status: http.StatusServiceUnavailable}, unreachable},
		{&apiError{Status: http.StatusUnauthorized, Code: "RESOURCE_ACCESS_DENIED", Message: "Not authenticated"}, refused},
		{&apiError{Status: http.StatusBadRequest, Code: "AUTHENTICATION_ERROR", Message: "Bad credentials"}, refused},
		// pyoverkiz's login reads this as bad credentials. It isn't.
		{&apiError{Status: http.StatusBadRequest, Code: "AUTHENTICATION_ERROR", Message: "Too many requests, try again later"}, limited},
		{&apiError{Status: http.StatusTooManyRequests}, limited},
		{&apiError{Status: http.StatusBadRequest, Code: "UNSPECIFIED_ERROR", Message: "No registered event listener"}, noListener},
		{&apiError{Status: http.StatusBadRequest, Code: "EXEC_ERROR", Message: "Unknown command"}, rejected},
	} {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("%v: got %d, want %d", tc.err, got, tc.want)
		}
	}
}
