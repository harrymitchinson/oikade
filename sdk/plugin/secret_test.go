package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/secretuse"
)

func TestASecretReadsItsValueEachTime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(contractv1.EnvSecretsDir, dir)
	s := NewSecret("weather-api-key")
	if s.Name() != "weather-api-key" {
		t.Errorf("Name() = %q", s.Name())
	}
	for _, value := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(dir, "weather-api-key"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := s.Read()
		if err != nil || got != value {
			t.Errorf("Read() = %q, %v, want %q", got, err, value)
		}
	}
}

func TestMakingASecretDeclaresIt(t *testing.T) {
	NewSecret("declared-by-making")
	if !slices.Contains(secretuse.Names(), "declared-by-making") {
		t.Errorf("declared %q", secretuse.Names())
	}
}

// A secret nobody set says how to set it, as under oikade dev, where there's no directory at all.
func TestASecretNotSetSaysHowToSetIt(t *testing.T) {
	for name, dir := range map[string]string{"empty directory": t.TempDir(), "no directory": ""} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(contractv1.EnvSecretsDir, dir)
			_, err := NewSecret("cozytouch").Read()
			if !errors.Is(err, ErrSecretNotSet) {
				t.Fatalf("err = %v, want ErrSecretNotSet", err)
			}
			if want := "secret cozytouch isn't set: oikade set secret/cozytouch"; err.Error() != want {
				t.Errorf("err = %q, want %q", err, want)
			}
		})
	}
}

func TestASecretWithNoGoodNameIsntRead(t *testing.T) {
	t.Setenv(contractv1.EnvSecretsDir, t.TempDir())
	for _, s := range []Secret{{}, NewSecret("../escape")} {
		if _, err := s.Read(); err == nil || errors.Is(err, ErrSecretNotSet) {
			t.Errorf("%q: err = %v", s.Name(), err)
		}
	}
}
