package host

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/fault"
)

// SecretsDir is the secrets' directory inside the data directory, so a backup has them.
const SecretsDir = "secrets"

// secrets is a directory with one file for each secret a plugin's setup keeps, such as a device's
// key, which the plugin reads. Your values aren't here: they come with a deploy (vars.go). Nothing
// here passes a value on, and nothing here logs one.
type secrets struct {
	dir string
}

// set keeps value as the secret name, on disk before it returns.
func (s secrets) set(name, value string) error {
	// The contract holds a setup to this too, so a name can't leave the directory.
	if !contractv1.ValidSecretName(name) {
		return fmt.Errorf("%q isn't a secret's name", name)
	}
	f, err := os.CreateTemp(s.dir, ".new-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	// CreateTemp makes it 0600, so the value is never readable by anyone else, even for a moment.
	_, err = f.WriteString(value)
	if err == nil {
		err = f.Sync()
	}
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, name))
	}
	if err == nil {
		// The rename is on disk only once the directory is.
		err = syncDir(s.dir)
	}
	if err == nil {
		fault.Hit(fault.HostSecretSaved)
	}
	return err
}

// WithBrokerPassword puts the password from the file name in dir into a broker's URL. A password
// belongs in a file, because the flags end up in a unit file and in ps.
func WithBrokerPassword(broker, dir, name string) (string, error) {
	u, err := url.Parse(broker)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("--broker-password-secret needs a user in --broker, such as mqtt://oikade@broker:1883")
	}
	if !contractv1.ValidSecretName(name) {
		return "", fmt.Errorf("%q isn't a secret's name", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("reading the broker's password: %w", err)
	}
	u.User = url.UserPassword(u.User.Username(), strings.TrimRight(string(raw), "\r\n"))
	return u.String(), nil
}

// redacted is a broker's URL for a log. One that doesn't parse says nothing, as it may hold a
// password.
func redacted(broker string) string {
	u, err := url.Parse(broker)
	if err != nil {
		return "(not a URL)"
	}
	return u.Redacted()
}
