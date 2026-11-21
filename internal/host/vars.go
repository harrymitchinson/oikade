package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
)

const varsFileName = "vars.json"

// varSet is a set of your values, with a digest that changes when any of them does, so that the
// plugins know to start again. Nothing logs the digest, which a guess at a value could be checked
// against.
type varSet struct {
	values map[string]string
	digest string
}

func newVarSet(values map[string]string) *varSet {
	sum := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(values)) {
		// A name can't hold a NUL, and a value's length goes first, so no two sets write the same.
		_, _ = fmt.Fprintf(sum, "%s\x00%d\x00%s", name, len(values[name]), values[name])
	}
	return &varSet{values: values, digest: hex.EncodeToString(sum.Sum(nil))}
}

// varsFile is where the host writes your values for each process it starts from your binary, which
// it names to the process in wire.VarsEnv. Only the host's user can read it.
type varsFile struct {
	path string
	set  func() *varSet

	mu sync.Mutex
}

// varsDir is where the file goes: the runtime directory, which systemd keeps in memory, or the data
// directory if there's none. A copy left in the data directory by a host that had none is removed,
// and so is any file of values a host killed as it wrote one, or as a check ran, left in either.
func varsDir(runtimeDir, dataDir string) (string, error) {
	var left []string
	for _, dir := range []string{runtimeDir, dataDir} {
		if dir == "" {
			continue
		}
		for _, pattern := range []string{".vars-*", ".check-vars-*"} {
			found, err := filepath.Glob(filepath.Join(dir, pattern))
			if err != nil {
				return "", err
			}
			left = append(left, found...)
		}
	}
	if runtimeDir != "" {
		left = append(left, filepath.Join(dataDir, varsFileName))
	}
	for _, path := range left {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("removing the values left by the last host: %w", err)
		}
	}
	if runtimeDir == "" {
		return dataDir, nil
	}
	return runtimeDir, nil
}

// write writes the values as they are now, whole, by a rename, so a process never reads half of
// them. The file needn't survive a crash, as it's written again before each process starts.
func (f *varsFile) write() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	tmp, err := f.temp(".vars-*", f.set().values)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("writing your values: %w", err)
	}
	return nil
}

// current is the values as they are now, or none for a nil varsFile.
func (f *varsFile) current() map[string]string {
	if f == nil {
		return nil
	}
	return f.set().values
}

// forCheck writes values for a binary's check mode, in a file of their own beside the processes',
// and returns the environment that names it and how to remove it. A nil varsFile gives none.
func (f *varsFile) forCheck(values map[string]string) ([]string, func(), error) {
	if f == nil {
		return nil, func() {}, nil
	}
	tmp, err := f.temp(".check-vars-*", values)
	if err != nil {
		return nil, nil, err
	}
	return []string{wire.VarsEnv + "=" + tmp}, func() { _ = os.Remove(tmp) }, nil
}

// temp writes values to a new file beside the processes' one, named for pattern, and returns its
// path.
func (f *varsFile) temp(pattern string, values map[string]string) (string, error) {
	if values == nil {
		values = map[string]string{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	// CreateTemp makes it 0600, so no one else can read it, even for a moment.
	tmp, err := os.CreateTemp(filepath.Dir(f.path), pattern)
	if err != nil {
		return "", fmt.Errorf("writing your values: %w", err)
	}
	_, err = tmp.Write(raw)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("writing your values: %w", err)
	}
	return tmp.Name(), nil
}

// checkVarNames refuses a set with a name that breaks the rule, which a process couldn't ask for.
func checkVarNames(values map[string]string) error {
	errs := make([]error, 0, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		errs = append(errs, ident.CheckVarName(name))
	}
	return errors.Join(errs...)
}

// changedVars names the values that differ between two sets: added, removed or changed, sorted.
func changedVars(before, after map[string]string) []string {
	var names []string
	for name, value := range after {
		if was, ok := before[name]; !ok || was != value {
			names = append(names, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// missingVars are the names a version uses that a set has no value for, in order.
func missingVars(used []string, values map[string]string) []string {
	var out []string
	for _, name := range used {
		if _, ok := values[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// missingError says which values a version can't go ahead without, and whose set lacks them.
func missingError(names []string, whose string) string {
	verb := "isn't"
	if len(names) > 1 {
		verb = "aren't"
	}
	return fmt.Sprintf("the version uses %s, which %s among %s values", joinNames(names), verb, whose)
}

// joinNames is "A", "A and B" or "A, B and C".
func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
