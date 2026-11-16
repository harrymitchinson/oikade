package host

import (
	"testing"

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
	if len(renamed) != 2 || renamed[0] != "Cottage" || renamed[1] != DefaultMatterName {
		t.Errorf("renamed %q, want once to Cottage and once back", renamed)
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
