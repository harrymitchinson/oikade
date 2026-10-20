// Package identity generates and validates HomeKit bridge identities.
package identity

import (
	"crypto/rand"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	usernameRe = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)
	pinRe      = regexp.MustCompile(`^\d{3}-\d{2}-\d{3}$`)
)

// HAP rejects these setup codes as too easy to guess.
var trivialPins = map[string]bool{
	"000-00-000": true, "111-11-111": true, "222-22-222": true, "333-33-333": true,
	"444-44-444": true, "555-55-555": true, "666-66-666": true, "777-77-777": true,
	"888-88-888": true, "999-99-999": true, "123-45-678": true, "876-54-321": true,
}

// NewUsername returns a random, locally administered unicast MAC address in the
// upper-case colon form HomeKit uses for a bridge identity.
func NewUsername() (string, error) {
	return newUsername(rand.Reader)
}

func newUsername(r io.Reader) (string, error) {
	b := make([]byte, 6)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	b[0] = (b[0] | 0x02) &^ 0x01
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, ":"), nil
}

// ValidUsername reports whether s is a well-formed bridge username.
func ValidUsername(s string) bool {
	return usernameRe.MatchString(s)
}

// ValidatePin checks a HomeKit setup code.
func ValidatePin(pin string) error {
	if !pinRe.MatchString(pin) {
		return fmt.Errorf("setup code must have the form XXX-XX-XXX")
	}
	if trivialPins[pin] {
		return fmt.Errorf("setup code %s is not allowed by HomeKit", pin)
	}
	return nil
}
