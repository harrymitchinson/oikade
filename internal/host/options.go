package host

import (
	"sync"

	"github.com/harrymitchinson/oikade/internal/matterbridge"
	"github.com/harrymitchinson/oikade/internal/store"
)

// DefaultMatterName is the Matter bridge's name until a home names it, with oikade.Options'
// MatterName.
const DefaultMatterName = "Oikade"

const (
	// optionsNamespace is where the store keeps the home's own settings, as its last process
	// declared them, so that a host that restarts applies them before any process connects.
	optionsNamespace = "options"
	matterNameKey    = "matterName"
)

// homeOptions are the settings a home declares in oikade.Options that the host applies, rather
// than its process.
type homeOptions struct {
	values *store.Namespace
	// renamed, if set, is given the bridge's name once it's saved, on every connect, so that the
	// home's code wins over a controller that wrote another. It must not block for long.
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
// saved before it's applied, so a host that restarts serves the name the running code gave, and
// applied even if it's the saved one, since a controller may have written the NodeLabel since.
func (o *homeOptions) takeMatterName(name string) error {
	if name == "" {
		name = DefaultMatterName
	}
	if err := matterbridge.CheckBridgeName(name); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if name != o.matterName {
		if err := o.values.Put(matterNameKey, []byte(name)); err != nil {
			return err
		}
		o.matterName = name
	}
	if o.renamed != nil {
		o.renamed(name)
	}
	return nil
}
