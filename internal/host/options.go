package host

import (
	"errors"
	"sync"

	"github.com/harrymitchinson/oikade/internal/store"
)

// DefaultMatterName is the Matter bridge's name until a home names it, with framework.Options'
// MatterName.
const DefaultMatterName = "Oikade"

const (
	// optionsNamespace is where the store keeps the home's own settings, as its last process
	// declared them, so that a host that restarts applies them before any process connects.
	optionsNamespace = "options"
	matterNameKey    = "matterName"
	// maxMatterName is the most a NodeLabel holds, in bytes.
	maxMatterName = 32
)

// homeOptions are the settings a home declares in framework.Options that the host applies, rather
// than its process.
type homeOptions struct {
	values *store.Namespace
	// renamed, if set, hears of a new bridge name once it's saved. It must not block for long.
	renamed func(string)

	mu         sync.Mutex
	matterName string
}

// loadOptions reads what the home last declared.
func loadOptions(values *store.Namespace) (*homeOptions, error) {
	raw, ok, err := values.Get(matterNameKey)
	if err != nil {
		return nil, err
	}
	o := &homeOptions{values: values, matterName: DefaultMatterName}
	if ok {
		o.matterName = string(raw)
	}
	return o, nil
}

// bridgeName is the Matter bridge's name.
func (o *homeOptions) bridgeName() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.matterName
}

// takeMatterName takes up a connecting process's bridge name, where empty is DefaultMatterName. It's
// saved before it's applied, so a host that restarts serves the name the running code gave.
func (o *homeOptions) takeMatterName(name string) error {
	if name == "" {
		name = DefaultMatterName
	}
	if len(name) > maxMatterName {
		return errors.New("the Matter bridge's name is longer than 32 bytes")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if name == o.matterName {
		return nil
	}
	if err := o.values.Put(matterNameKey, []byte(name)); err != nil {
		return err
	}
	o.matterName = name
	if o.renamed != nil {
		o.renamed(name)
	}
	return nil
}
