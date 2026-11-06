package host

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// DefaultSecretsDir is the secrets' directory inside the data directory, so a backup has them.
const DefaultSecretsDir = "secrets"

// secretName is what a secret can be called, which can't leave the directory or hide in it.
var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// secrets is a directory with one file per credential. Your process and plugins read the files, so
// nothing here passes a value on, and nothing here logs one.
type secrets struct {
	dir string
}

func (s secrets) list() ([]api.Secret, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var found []api.Secret
	for _, e := range entries {
		// A dot-named file is a set that a crash cut short.
		if !secretName.MatchString(e.Name()) {
			continue
		}
		info, err := os.Stat(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			found = append(found, api.Secret{Name: e.Name(), Changed: info.ModTime().UTC()})
		}
	}
	return found, nil
}

func (s secrets) set(name, value string) error {
	f, err := os.CreateTemp(s.dir, ".new-*")
	if err != nil {
		return writable(err)
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
	return writable(err)
}

func (s secrets) remove(name string) error {
	err := os.Remove(filepath.Join(s.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no secret %s", name)
	}
	return writable(err)
}

// writable explains the error a mounted directory gives.
func writable(err error) error {
	if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
		return errors.New("the host's secrets directory is read-only, so change them where it's mounted from")
	}
	return err
}

func (s *apiServer) secret(req api.Request) api.Response {
	if req.Type == api.TypeSecrets {
		found, err := s.secrets.list()
		if err != nil {
			return api.Response{Error: err.Error()}
		}
		return api.Response{Secrets: found}
	}
	if !secretName.MatchString(req.Secret) {
		return api.Response{Error: fmt.Sprintf("%q isn't a secret's name: use letters, digits, '.', '_' and '-'", req.Secret)}
	}
	var err error
	if req.Type == api.TypeSecretSet {
		err = s.secrets.set(req.Secret, req.SecretValue)
	} else {
		err = s.secrets.remove(req.Secret)
	}
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	s.log.Info("Changed secret", "name", req.Secret)
	return api.Response{}
}

// WithBrokerPassword puts the password from secret name into a broker's URL. A password belongs in a
// secret, because the flags end up in a unit file and in ps.
func WithBrokerPassword(broker, dir, name string) (string, error) {
	u, err := url.Parse(broker)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("--broker-password-secret needs a user in --broker, such as mqtt://oikade@broker:1883")
	}
	if !secretName.MatchString(name) {
		return "", fmt.Errorf("%q isn't a secret's name", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("reading the broker's password: %w", err)
	}
	u.User = url.UserPassword(u.User.Username(), strings.TrimRight(string(raw), "\r\n"))
	return u.String(), nil
}

// redacted is a broker's URL for a log. One that doesn't parse says nothing, as it may hold a password.
func redacted(broker string) string {
	u, err := url.Parse(broker)
	if err != nil {
		return "(not a URL)"
	}
	return u.Redacted()
}
