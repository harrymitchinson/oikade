package overkiz

import (
	"errors"
	"net/http"
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

func TestEveryModelServesOnlyKnownCapabilities(t *testing.T) {
	for widget, m := range models {
		for _, b := range m.bindings {
			if b.read == nil || b.state == "" {
				t.Errorf("%s: %s has no state to read", widget, b.capability)
			}
			switch b.capability {
			case contractv1.Position:
			default:
				t.Errorf("%s: capability %s isn't checked here", widget, b.capability)
			}
		}
	}
	if _, ok := closure.bindings[0].read(101.0); ok {
		t.Error("a closure of 101 was read")
	}
	if got, err := closure.bindings[0].write(42.6); err != nil || len(got) != 1 || got[0].Name != "setClosure" || got[0].Parameters[0] != 43 {
		t.Errorf("writing 42.6: %+v, %v", got, err)
	}
	// Anything else is refused rather than sent, so it can't open a shutter as 0 would.
	for _, v := range []any{true, 150.0, -1.0, "50"} {
		if got, err := closure.bindings[0].write(v); err == nil {
			t.Errorf("writing %v: %+v", v, got)
		}
	}
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
