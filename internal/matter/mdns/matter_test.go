package mdns

import (
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

func TestCommissionable(t *testing.T) {
	c := Commissionable{
		Instance:      "B7322C948581262F",
		Discriminator: 3840,
		VendorID:      0xFFF1,
		ProductID:     0x8000,
		Mode:          CommissioningBasic,
		DeviceType:    0x000E, // aggregator
		DeviceName:    "Oikade",
		PairingHint:   33,
		MRP:           transport.MRPConfig{Idle: 500 * time.Millisecond, Active: 300 * time.Millisecond, ActiveThreshold: 4 * time.Second},
	}
	svc, err := c.Service()
	if err != nil {
		t.Fatal(err)
	}
	if svc.Type != "_matterc._udp" || svc.Port != 5540 || svc.Instance != c.Instance {
		t.Errorf("service %s.%s port %d", svc.Instance, svc.Type, svc.Port)
	}
	if want := []string{"_L3840", "_S15", "_V65521", "_T14", "_CM"}; !slices.Equal(svc.Subtypes, want) {
		t.Errorf("subtypes %q, want %q", svc.Subtypes, want)
	}
	want := []string{"D=3840", "CM=1", "VP=65521+32768", "DT=14", "DN=Oikade", "SII=500", "SAI=300", "SAT=4000", "PH=33"}
	if !slices.Equal(svc.Text, want) {
		t.Errorf("TXT %q, want %q", svc.Text, want)
	}

	// With the window closed, and only what's required.
	svc, err = Commissionable{Instance: "B7322C948581262F", Discriminator: 0x0A5, TCP: 6}.Service()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"_L165", "_S0"}; !slices.Equal(svc.Subtypes, want) {
		t.Errorf("subtypes %q, want %q", svc.Subtypes, want)
	}
	if want := []string{"D=165", "CM=0", "T=6"}; !slices.Equal(svc.Text, want) {
		t.Errorf("TXT %q, want %q", svc.Text, want)
	}

	for _, bad := range []Commissionable{
		{Instance: "B7322C948581262F", Discriminator: 4096},
		{Instance: "B7322C948581262F", DeviceName: "a device name that is longer than 32 bytes"},
	} {
		if _, err := bad.Service(); err == nil {
			t.Errorf("%+v is valid", bad)
		}
	}
}

func TestOperational(t *testing.T) {
	svc := Operational{
		CompressedFabricID: 0x87E1B004E235A130,
		NodeID:             0x8FC7772401CD0696,
		MRP:                transport.MRPConfig{Idle: 2 * time.Hour},
	}.Service()
	if svc.Instance != "87E1B004E235A130-8FC7772401CD0696" || svc.Type != "_matter._tcp" || svc.Port != 5540 {
		t.Errorf("service %s.%s port %d", svc.Instance, svc.Type, svc.Port)
	}
	if want := []string{"_I87E1B004E235A130"}; !slices.Equal(svc.Subtypes, want) {
		t.Errorf("subtypes %q, want %q", svc.Subtypes, want)
	}
	if want := []string{"SII=3600000"}; !slices.Equal(svc.Text, want) { // capped at an hour
		t.Errorf("TXT %q, want %q", svc.Text, want)
	}
	if err := svc.validate(); err != nil {
		t.Error(err)
	}
	if got := (Operational{NodeID: 1}).Service().Instance; got != "0000000000000000-0000000000000001" {
		t.Errorf("instance %q isn't zero-padded", got)
	}
}

func TestNewInstanceName(t *testing.T) {
	a, b := NewInstanceName(), NewInstanceName()
	if !regexp.MustCompile(`^[0-9A-F]{16}$`).MatchString(a) || a == b {
		t.Errorf("instance names %q and %q", a, b)
	}
}
