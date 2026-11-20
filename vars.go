package oikade

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"

	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// Var is the value called name in your home's .env, the file beside its go.mod that Git ignores,
// which oikade deploy sends the host with your binary (OIK-365). It's for what's yours rather than
// your home's, such as a login's username and password or a hub's address, so the code can be
// pushed or shared as it stands:
//
//	home.Plugin("toshiba", toshibaac.New(toshibaac.Config{Username: oikade.Var("TOSHIBA_USER")}))
//
// A name is the same in your code, the file and the environment, and follows an environment
// variable's rule: upper-case letters, digits and underscores, starting with a letter. Call Var where
// the home is built, such as in a plugin's config or a package-level var: that declares it, so
// oikade deploy refuses to go ahead without a value for it, and one called in a rule as it runs is
// too late for that. The host gives the values to each process it starts from your binary, and
// keeps them as it keeps secrets: never in a binary, a log or an answer. Built or tested without
// the host, such as by go test or in oikade check, a Var is empty.
func Var(name string) string {
	return hostVars.get(name)
}

// hostVars are the values the host gave this process, and the names the home's code used.
var hostVars = &values{path: func() string { return os.Getenv(wire.VarsEnv) }}

type values struct {
	path func() string

	mu   sync.Mutex
	used map[string]bool
	read bool
	set  map[string]string
	err  error
}

func (v *values) get(name string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.used == nil {
		v.used = map[string]bool{}
	}
	v.used[name] = true
	if !v.read {
		v.read = true
		v.set, v.err = readValues(v.path())
	}
	return v.set[name]
}

// names are the names the home's code used, sorted.
func (v *values) names() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Sorted(maps.Keys(v.used))
}

// check says which of the names used break the rule, and why the host's values couldn't be read.
func (v *values) check() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(v.used)) {
		if err := ident.CheckVarName(name); err != nil {
			errs = append(errs, fmt.Errorf("oikade.Var: %w", err))
		}
	}
	if v.err != nil {
		errs = append(errs, fmt.Errorf("reading the values the host gave this process: %w", v.err))
	}
	return errors.Join(errs...)
}

// readValues reads the file the host named, or nothing if it named none.
func readValues(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set map[string]string
	if err := json.Unmarshal(raw, &set); err != nil {
		// Not the error's text, which could quote a value.
		return nil, errors.New("the file isn't a JSON object of strings")
	}
	return set, nil
}
