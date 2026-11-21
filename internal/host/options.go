package host

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/harrymitchinson/oikade/internal/fault"
	"github.com/harrymitchinson/oikade/internal/host/wire"
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
	hiddenKey        = "hiddenFromMatter"
)

// homeOptions are the settings a home declares in oikade.Options that the host applies, rather
// than its process.
type homeOptions struct {
	values *store.Namespace
	// renamed, if set, is given the bridge's name once it's saved, on every connect, so that the
	// home's code wins over a controller that wrote another. It must not block for long.
	renamed func(string)
	// rehidden, if set, hears that the plugins hidden from Matter changed, once that's saved. It
	// must not block.
	rehidden func()

	mu         sync.Mutex
	matterName string
	hidden     []wire.HiddenPlugin
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
	// One that can't be read is an error rather than none, which would put every hidden device back
	// in the Home app twice.
	if raw, ok, err = values.Get(hiddenKey); err != nil {
		return nil, err
	} else if ok {
		if err := json.Unmarshal(raw, &o.hidden); err != nil {
			return nil, fmt.Errorf("reading the plugins hidden from Matter: %w", err)
		}
	}
	return o, nil
}

// hiddenFromMatter are the plugins the home keeps off Matter. Nil options hide none.
func (o *homeOptions) hiddenFromMatter() []wire.HiddenPlugin {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hidden
}

// takeHidden takes up the plugins a connecting process's home hides from Matter. They're saved
// before they're applied, so a host that restarts keeps off Matter what the running code keeps off.
func (o *homeOptions) takeHidden(hidden []wire.HiddenPlugin) error {
	hidden = normalHidden(hidden)
	o.mu.Lock()
	defer o.mu.Unlock()
	if slices.EqualFunc(hidden, o.hidden, func(a, b wire.HiddenPlugin) bool {
		return a.Plugin == b.Plugin && slices.Equal(a.Shown, b.Shown)
	}) {
		return nil
	}
	var err error
	if len(hidden) == 0 {
		err = o.values.Delete(hiddenKey)
	} else {
		var raw []byte
		if raw, err = json.Marshal(hidden); err == nil {
			err = o.values.Put(hiddenKey, raw)
		}
	}
	if err != nil {
		return err
	}
	fault.Hit(fault.HostOptionsSaved)
	o.hidden = hidden
	if o.rehidden != nil {
		o.rehidden()
	}
	return nil
}

// normalHidden is hidden in order, by plugin and then by device, so that reordering a home's code
// changes nothing. Nil hides none.
func normalHidden(hidden []wire.HiddenPlugin) []wire.HiddenPlugin {
	if len(hidden) == 0 {
		return nil
	}
	out := make([]wire.HiddenPlugin, len(hidden))
	for i, h := range hidden {
		out[i] = wire.HiddenPlugin{Plugin: h.Plugin, Shown: slices.Sorted(slices.Values(h.Shown))}
	}
	slices.SortFunc(out, func(a, b wire.HiddenPlugin) int { return strings.Compare(a.Plugin, b.Plugin) })
	return out
}

func (o *homeOptions) bridgeName() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.matterName
}

// takeMatterName takes up a connecting process's bridge name, where empty is DefaultMatterName.
// It's saved before it's applied, so a host that restarts serves the name the running code gave,
// and applied even if it's the saved one, since a controller may have written the NodeLabel since.
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
		fault.Hit(fault.HostOptionsSaved)
		o.matterName = name
	}
	if o.renamed != nil {
		o.renamed(name)
	}
	return nil
}
