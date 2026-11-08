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
	// Removals is rules the running version doesn't have, whose claims and state are yet to go.
	Removals bool `json:"removals,omitempty"`
	// Spared is the rules with anything on record when the deploy in progress began. A failed
	// version made none of that, and the check never looked at it, so its clean-up leaves it.
	Spared []string `json:"spared,omitempty"`
}

// versions keeps your binaries. Only one deploy runs at a time, so it needs no lock of its own.
type versions struct {
	dir string
	// uploads is where deploys arrive, which a version takes by renaming rather than copying.
	uploads string
	values  *store.Namespace
	state   history
}

func openVersions(dataDir string, data *store.Store) (*versions, error) {
	v := &versions{
		dir: filepath.Join(dataDir, versionsDir), uploads: filepath.Join(dataDir, uploadsDir),
		values: data.Namespace(versionsNamespace),
	}
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

// save records next, and takes it up only once that's on disk, so a failed save changes nothing.
func (v *versions) save(next history) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := v.values.Put(versionsKey, raw); err != nil {
		return err
	}
	v.state = next
	return nil
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

// take stores a binary, as add does. One that arrived as an upload is moved, not copied, as it's on
// the same disk and nothing else wants it.
func (v *versions) take(path string) (string, error) {
	if v.uploads == "" || filepath.Dir(path) != v.uploads {
		return v.add(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	_, err = io.Copy(sum, f)
	if err == nil {
		err = f.Chmod(0o700)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	id := hex.EncodeToString(sum.Sum(nil))[:32]
	if err := os.Rename(path, v.path(id)); err != nil {
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
	return v.save(history{Versions: []version{next}, Removals: v.state.Removals, Spared: v.state.Spared})
}

// begin makes next the running version, unconfirmed. Call it before the process changes.
func (v *versions) begin(next version, spared []string) error {
	rest := slices.DeleteFunc(slices.Clone(v.state.Versions), func(k version) bool { return k.ID == next.ID })
	return v.pending(append([]version{next}, rest...), slices.Clone(v.state.Versions), spared)
}

// beginFromHome is begin for a first deploy, over a binary from the host's flags that isn't on record.
// One write records both, so the home binary is on record only if the deploy went ahead, and a crash
// after it still has the home binary to go back to.
func (v *versions) beginFromHome(home, next version, spared []string) error {
	return v.pending([]version{next, home}, []version{home}, spared)
}

// beginRollback makes next, the version before the running one, the running one, unconfirmed. The
// running one leaves the history, so that another rollback goes back further rather than to it.
func (v *versions) beginRollback(next version, spared []string) error {
	return v.pending(append([]version{next}, v.state.Versions[2:]...), slices.Clone(v.state.Versions), spared)
}

func (v *versions) pending(next, before []version, spared []string) error {
	return v.save(history{
		Versions: next, Before: before, Pending: true,
		Removals: v.state.Removals, Spared: spared,
	})
}

// commit confirms the running version, which leaves its removals to be dealt with.
func (v *versions) commit() error {
	return v.save(history{
		Versions: slices.Clone(v.state.Versions[:min(len(v.state.Versions), keptVersions)]), Removals: true,
	})
}

// removalsDone records that a confirmed version's removals have been dealt with.
func (v *versions) removalsDone() error {
	next := v.state
	next.Removals, next.Spared = false, nil
	return v.save(next)
}

// abort goes back to how things were before begin, and removes the failed binary. The failed version
// may have left rules of its own, so removals are due, sparing what was there before it. It goes back even if that can't be saved,
// because it's no longer what runs, and the next start will find the deploy unfinished.
func (v *versions) abort() error {
	failed := v.state.Versions[0].ID
	next := history{Versions: v.state.Before, Removals: true, Spared: v.state.Spared}
	err := v.save(next)
	v.state = next
	if err != nil {
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
