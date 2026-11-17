package toshibaac

import (
	"testing"
	"time"
)

func TestASASTokenNamesItsHubAndDevice(t *testing.T) {
	tok, err := parseSAS("SharedAccessSignature sr=example-hub.azure-devices.net%2Fdevices%2Fsomeone%2540example.com_0123&sig=a%2Bb%3D&se=1800000000")
	if err != nil {
		t.Fatal(err)
	}
	if tok.host != "example-hub.azure-devices.net" || tok.device != "someone@example.com_0123" {
		t.Errorf("host %q, device %q", tok.host, tok.device)
	}
	if !tok.expires.Equal(time.Unix(1800000000, 0)) {
		t.Errorf("expires %v", tok.expires)
	}

	// Without an expiry, it doesn't run out.
	tok, err = parseSAS("SharedAccessSignature sr=hub%2Fdevices%2Fd&sig=x")
	if err != nil || !tok.expires.IsZero() || tok.device != "d" {
		t.Errorf("%+v, %v", tok, err)
	}

	for _, bad := range []string{
		"SharedAccessSignature sig=x",
		"SharedAccessSignature sr=hub%2Fd&sig=x",
		"SharedAccessSignature sr=%2Fdevices%2Fd",
		"SharedAccessSignature sr=hub%2Fdevices%2F",
		"SharedAccessSignature sr=hub%2Fdevices%2Fd&se=soon",
		"SharedAccessSignature sr=%zz",
	} {
		if _, err := parseSAS(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestAMethodCallsTopicNamesItAndItsRequest(t *testing.T) {
	name, rid, ok := parseMethod("$iothub/methods/POST/smmobile/?$rid=7")
	if !ok || name != "smmobile" || rid != "7" {
		t.Errorf("%q %q %v", name, rid, ok)
	}
	for _, bad := range []string{
		"$iothub/twin/res/200/?$rid=1",
		"$iothub/methods/POST/smmobile",
		"$iothub/methods/POST/smmobile/?other=1",
	} {
		if _, _, ok := parseMethod(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestAUnitsIDIsItsUniqueIDAsADeviceID(t *testing.T) {
	tests := []struct {
		unique, want string
		ok           bool
	}{
		{"A1B2C3D4E5F6", "a1b2c3d4e5f6", true},
		{" 0123-4567 ", "0123-4567", true},
		{"ab:cd:ef", "ab-cd-ef", true},
		{"-leading", "leading", true},
		{"", "", false},
		{"::", "", false},
	}
	for _, tt := range tests {
		got, ok := deviceID(tt.unique)
		if got != tt.want || ok != tt.ok {
			t.Errorf("deviceID(%q) = %q, %v; want %q, %v", tt.unique, got, ok, tt.want, tt.ok)
		}
	}
	if long, ok := deviceID("x234567890123456789012345678901234567890123456789012345678901234567890"); !ok || len(long) != 63 {
		t.Errorf("a long ID is %q, %v", long, ok)
	}
}

func TestTheClientIDIsTheAccountsEachTime(t *testing.T) {
	a := Config{Username: "someone@example.com"}.clientID()
	if a != (Config{Username: "someone@example.com"}).clientID() {
		t.Error("the client ID changed")
	}
	if a == (Config{Username: "other@example.com"}).clientID() {
		t.Error("two accounts share a client ID")
	}
	if want := len("someone@example.com_") + 16; len(a) != want {
		t.Errorf("client ID %q", a)
	}
}
