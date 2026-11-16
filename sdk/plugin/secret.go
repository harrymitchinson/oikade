package plugin

import (
	"errors"
	"fmt"
	"os"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/secretuse"
)

// ErrSecretNotSet is Secret.Read's error for a secret nobody has set with oikade secret set.
var ErrSecretNotSet = errors.New("secret isn't set")

// Secret is a credential kept on the host, such as a password or an API key, which oikade secret
// set sets. A home makes one with oikade.Secret and passes it where it's used, such as a plugin's
// config. Making one declares it, so oikade deploy and oikade status name it while it isn't set.
// Make them where the home is built, such as in a package-level var or the plugin's config, rather
// than in a rule as it runs, which is too late for the host to know.
//
// A Secret holds only its name: Read reads the value each time, so a changed secret needs no
// deploy. Its zero value names no secret.
type Secret struct {
	name string
}

// NewSecret makes a Secret and declares it. A home calls oikade.Secret, which is the same: this
// is for a plugin's own tests and for code that has no home.
func NewSecret(name string) Secret {
	secretuse.Use(name)
	return Secret{name: name}
}

// Name is the secret's name, as oikade secret set takes it. It's empty for the zero Secret.
func (s Secret) Name() string { return s.name }

// Read reads the secret's value from the host. A secret nobody has set, as under oikade dev, is an
// error that wraps ErrSecretNotSet and names the command that sets it. Never log the value.
func (s Secret) Read() (string, error) {
	if s.name == "" {
		return "", errors.New("the secret has no name: make it with oikade.Secret")
	}
	if !contractv1.ValidSecretName(s.name) {
		return "", fmt.Errorf("secret %q isn't a valid name", s.name)
	}
	value, ok, err := ReadSecret(Environment{SecretsDir: os.Getenv(contractv1.EnvSecretsDir)}, s.name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", notSetError(s)
	}
	return value, nil
}

// notSetError is a secret that isn't set, and says how to set it.
type notSetError struct{ name string }

func (e notSetError) Error() string {
	return fmt.Sprintf("secret %s isn't set: oikade secret set %s", e.name, e.name)
}

func (e notSetError) Is(target error) bool { return target == ErrSecretNotSet }
