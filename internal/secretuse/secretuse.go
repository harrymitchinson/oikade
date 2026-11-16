// Package secretuse keeps the names of the secrets a home's code uses, so the manifest can list
// them. plugin.NewSecret adds to it, and framework reads it.
package secretuse

import (
	"slices"
	"sync"
)

var (
	mu    sync.Mutex
	names = map[string]bool{}
)

// Use records that the home's code uses the secret name.
func Use(name string) {
	mu.Lock()
	defer mu.Unlock()
	names[name] = true
}

// Names are the secrets used so far, sorted.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
