package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	// versionsDir holds your binaries in the data directory.
	versionsDir = "versions"
	// versionsNamespace is where the store keeps track of them.
	versionsNamespace = "versions"
	versionsKey       = "history"
	// keptVersions bounds the versions kept, the running one included.
	keptVersions = 5
)

// version is one build of your binary, named by its contents.
type version struct {
	ID       string        `json:"id"`
	Manifest wire.Manifest `json:"manifest"`
	At       time.Time     `json:"at"`
}

// history is what the store keeps. It's written before a process changes, so a crash at any point
// leaves it saying what to do next.
type history struct {
	// Versions has the running one first, then the ones before it.
	Versions []version `json:"versions"`
	// Before is what Versions was until the deploy in progress, which hasn't settled if Pending.
	Before  []version `json:"before,omitempty"`
	Pending bool      `json:"pending,omitempty"`
}

// versions keeps your binaries. Only one deploy runs at a time, so it needs no lock of its own.
type versions struct {
	dir    string
	values *store.Namespace
	state  history
}

func openVersions(dataDir string, data *store.Store) (*versions, error) {
	v := &versions{dir: filepath.Join(dataDir, versionsDir), values: data.Namespace(versionsNamespace)}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return nil, err
	}
	raw, ok, err := v.values.Get(versionsKey)
	if err != nil {
		return nil, err
	}
	if ok {
		if err := json.Unmarshal(raw, &v.state); err != nil {
			return nil, fmt.Errorf("reading the version history: %w", err)
		}
	}
	return v, nil
}

func (v *versions) save() error {
	raw, err := json.Marshal(v.state)
	if err != nil {
		return err
	}
	return v.values.Put(versionsKey, raw)
}

func (v *versions) path(id string) string { return filepath.Join(v.dir, id) }

// current returns the running version, if one has been deployed.
func (v *versions) current() (version, bool) {
	if len(v.state.Versions) == 0 {
		return version{}, false
	}
	return v.state.Versions[0], true
}

// previous returns the version before the running one.
func (v *versions) previous() (version, bool) {
	if len(v.state.Versions) < 2 {
		return version{}, false
	}
	return v.state.Versions[1], true
}

func (v *versions) known(id string) bool {
	return slices.ContainsFunc(v.state.Versions, func(k version) bool { return k.ID == id })
}

// add copies a binary into the data directory, and returns its ID. Its name is its contents, so
// adding the same build twice changes nothing.
func (v *versions) add(path string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(v.dir, ".incoming-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), src); err != nil {
		return "", err
	}
	if err := tmp.Chmod(0o700); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	id := hex.EncodeToString(sum.Sum(nil))[:32]
	if err := os.Rename(tmp.Name(), v.path(id)); err != nil {
		return "", err
	}
	return id, syncDir(v.dir)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// discard removes a binary no version in the history names.
func (v *versions) discard(id string) error {
	if v.known(id) {
		return nil
	}
	if err := os.Remove(v.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// adopt records the version running now, which nothing deployed, so that a deploy has something to
// go back to.
func (v *versions) adopt(next version) error {
	v.state.Versions = []version{next}
	return v.save()
}

// begin makes next the running version, unconfirmed. Call it before the process changes.
func (v *versions) begin(next version) error {
	v.state.Before = slices.Clone(v.state.Versions)
	rest := slices.DeleteFunc(slices.Clone(v.state.Versions), func(k version) bool { return k.ID == next.ID })
	v.state.Versions = append([]version{next}, rest...)
	v.state.Pending = true
	return v.save()
}

// commit confirms the running version, and removes what's past the limit.
func (v *versions) commit() error {
	v.state.Before, v.state.Pending = nil, false
	v.state.Versions = v.state.Versions[:min(len(v.state.Versions), keptVersions)]
	if err := v.save(); err != nil {
		return err
	}
	return v.prune()
}

// abort goes back to how things were before begin. It keeps a failed version's file out of the way.
func (v *versions) abort() error {
	failed := v.state.Versions[0].ID
	v.state.Versions, v.state.Before, v.state.Pending = v.state.Before, nil, false
	if err := v.save(); err != nil {
		return err
	}
	return v.discard(failed)
}

// prune removes binaries no version names.
func (v *versions) prune() error {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !v.known(e.Name()) {
			errs = append(errs, os.Remove(v.path(e.Name())))
		}
	}
	return errors.Join(errs...)
}
