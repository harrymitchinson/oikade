package setup

import (
	"strconv"
	"testing"
)

// Vectors from connectedhomeip's src/setup_payload/tests (TestQRCode.cpp, TestManualCode.cpp and
// TestHelpers.h).

func TestBase38(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want string
	}{
		{nil, ""},
		{[]byte{10}, "A0"},
		{[]byte{10, 10}, "OT10"},
		{[]byte{10, 10, 10}, "-N.B0"},
		{[]byte{10, 10, 0}, "OT100"},
		{[]byte{10, 10, 40}, "Y6V91"},
		{[]byte{10, 10, 41}, "KL0B1"},
		{[]byte{10, 10, 255}, "Q-M08"},
		{[]byte{35}, "Z0"},
		{[]byte{255, 0}, "R600"},
		{[]byte{46, 0, 0}, "81000"},
		{[]byte{255}, "R6"},
		{[]byte{255, 255}, "NE71"},
		{[]byte{255, 255, 255}, "PLS18"},
		{[]byte("Hello World!"), "KKHF3W2S013OPM3EJX11"},
	} {
		if got := Base38(c.in); got != c.want {
			t.Errorf("Base38(% x) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQRCode(t *testing.T) {
	p := Payload{VendorID: 12, ProductID: 1, Flow: FlowStandard, Discovery: DiscoverySoftAP, Discriminator: 128, Passcode: 2048}
	got, err := p.QRCode()
	if err != nil || got != "MT:M5L90MP500K64J00000" {
		t.Errorf("QRCode() = %q, %v; want MT:M5L90MP500K64J00000", got, err)
	}
}

func TestManualCode(t *testing.T) {
	// The expected strings are without their Verhoeff check digit, as in the source.
	for _, c := range []struct {
		p    Payload
		want string
	}{
		{Payload{Passcode: 12345679, Discriminator: 2560}, "2412950753"},
		{Payload{Passcode: 12345679, Discriminator: 2560, Flow: FlowCustom}, "64129507530000000000"},
		{Payload{Passcode: 12345679, Discriminator: 2560, Flow: FlowCustom, VendorID: 1, ProductID: 1}, "64129507530000100001"},
		{Payload{Passcode: 12345679, Discriminator: 2560, VendorID: 45367, ProductID: 14526}, "2412950753"},
		{Payload{Passcode: 12345679, Discriminator: 2560, Flow: FlowCustom, VendorID: 45367, ProductID: 14526}, "64129507534536714526"},
		// Only the discriminator's top 4 bits.
		{Payload{Passcode: 12345679, Discriminator: 0xa1f}, "2412950753"},
	} {
		got, err := c.p.ManualCode()
		if err != nil {
			t.Fatalf("%+v: %v", c.p, err)
		}
		if want := c.want + strconv.Itoa(Verhoeff(c.want)); got != want {
			t.Errorf("%+v: ManualCode() = %s, want %s", c.p, got, want)
		}
	}
}

func TestVerhoeff(t *testing.T) {
	// Wikipedia's example, and a wrong check digit from connectedhomeip.
	if got := Verhoeff("236"); got != 3 {
		t.Errorf("Verhoeff(236) = %d, want 3", got)
	}
	if got := Verhoeff("0268435458"); got == 9 {
		t.Error("02684354589 has the wrong check digit, but Verhoeff says it's right")
	}
}

func TestInvalidPayloads(t *testing.T) {
	for _, p := range []Payload{
		{Passcode: 0},
		{Passcode: 12345678},
		{Passcode: 99999999},
		{Passcode: 100000000},
		{Passcode: 1, Discriminator: 4096},
		{Passcode: 1, Version: 8},
		{Passcode: 1, Flow: 3},
	} {
		if _, err := p.QRCode(); err == nil {
			t.Errorf("QRCode accepted %+v", p)
		}
		if _, err := p.ManualCode(); err == nil {
			t.Errorf("ManualCode accepted %+v", p)
		}
	}
}

// TestDefaultTestDevice is connectedhomeip's default test device (passcode 20202021,
// discriminator 3840), whose manual code its docs give in full, check digit included.
func TestDefaultTestDevice(t *testing.T) {
	p := Payload{Passcode: 20202021, Discriminator: 3840}
	if got, err := p.ManualCode(); err != nil || got != "34970112332" {
		t.Errorf("ManualCode() = %s, %v; want 34970112332", got, err)
	}
}
