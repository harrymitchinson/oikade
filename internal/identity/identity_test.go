package identity

import (
	"bytes"
	"testing"
)

func TestNewUsername(t *testing.T) {
	u, err := newUsername(bytes.NewReader([]byte{0xFF, 0x00, 0xAB, 0xCD, 0xEF, 0x01}))
	if err != nil {
		t.Fatal(err)
	}
	// Multicast bit cleared, locally administered bit set.
	if want := "FE:00:AB:CD:EF:01"; u != want {
		t.Fatalf("got %s, want %s", u, want)
	}

	for range 100 {
		u, err := NewUsername()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidUsername(u) {
			t.Fatalf("invalid username %q", u)
		}
	}
}

func TestValidatePin(t *testing.T) {
	for pin, ok := range map[string]bool{
		"031-45-154": true,
		"031-45-15":  false,
		"03145154":   false,
		"abc-de-fgh": false,
		"111-11-111": false,
		"123-45-678": false,
	} {
		if err := ValidatePin(pin); (err == nil) != ok {
			t.Errorf("ValidatePin(%q) = %v, want ok=%v", pin, err, ok)
		}
	}
}
