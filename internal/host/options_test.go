package host

import (
	"reflect"
	"slices"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/store"
)

func TestTheBridgesNameIsSavedBeforeItsApplied(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	o, err := loadOptions(data.Namespace(optionsNamespace))
	if err != nil {
		t.Fatal(err)
	}
	if got := o.bridgeName(); got != DefaultMatterName {
		t.Errorf("a host no home has named calls its bridge %q", got)
	}
	var renamed []string
	o.renamed = func(name string) {
		raw, _, err := data.Namespace(optionsNamespace).Get(matterNameKey)
		if err != nil || string(raw) != name {
			t.Errorf("renamed to %q with %q saved, %v", name, raw, err)
		}
		renamed = append(renamed, name)
	}
	for _, name := range []string{"Cottage", "Cottage", "", ""} {
		if err := o.takeMatterName(name); err != nil {
			t.Fatal(err)
		}
	}
	// Every connect applies the name, saved or not, in case a controller wrote another.
	if want := []string{"Cottage", "Cottage", DefaultMatterName, DefaultMatterName}; !slices.Equal(renamed, want) {
		t.Errorf("renamed %q, want %q", renamed, want)
	}
	if err := o.takeMatterName("a name that is longer than thirty-two bytes"); err == nil {
		t.Error("took a name too long for a NodeLabel")
	}
	if err := o.takeMatterName("Cottage"); err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	// A host that restarts names the bridge before any process connects.
	data, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	o, err = loadOptions(data.Namespace(optionsNamespace))
	if err != nil {
		t.Fatal(err)
	}
	if got := o.bridgeName(); got != "Cottage" {
		t.Errorf("after a restart, the bridge is named %q", got)
	}
}

func TestThePluginsHiddenFromMatterAreSavedBeforeTheyreApplied(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	values := data.Namespace(optionsNamespace)
	o, err := loadOptions(values)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.hiddenFromMatter(); got != nil {
		t.Errorf("a host no home has told hides %+v", got)
	}
	hidden := []wire.HiddenPlugin{{Plugin: "tahoma", Shown: []string{"garage-door"}}}
	changes := 0
	o.rehidden = func() {
		raw, ok, err := values.Get(hiddenKey)
		if err != nil || ok != (len(o.hidden) > 0) {
			t.Errorf("applied with %q saved, %v", raw, err)
		}
		changes++
	}
	for range 2 {
		if err := o.takeHidden(hidden); err != nil {
			t.Fatal(err)
		}
	}
	if changes != 1 {
		t.Errorf("the bridge heard of %d changes for one", changes)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	// A host that restarts keeps them off before any process connects.
	data, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	values = data.Namespace(optionsNamespace)
	if o, err = loadOptions(values); err != nil {
		t.Fatal(err)
	}
	if got := o.hiddenFromMatter(); !reflect.DeepEqual(got, hidden) {
		t.Errorf("after a restart, it hides %+v", got)
	}

	// A home that stops hiding them forgets it.
	if err := o.takeHidden(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := values.Get(hiddenKey); ok || o.hiddenFromMatter() != nil {
		t.Error("a home that hides nothing left them hidden")
	}

	// One that can't be read stops the host rather than putting every device back.
	if err := values.Put(hiddenKey, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptions(values); err == nil {
		t.Error("read a setting that can't be read as none")
	}
}
