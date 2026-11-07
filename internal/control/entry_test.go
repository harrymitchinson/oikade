package control

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The host keeps entries in its registry and reads them from --simulate files, so the format they
// write has to read back.
func TestEntryRoundTrips(t *testing.T) {
	const written = `{"capability":"on","default":false,"resolve":"PreferOff","external":"Override","overrideHold":"1h",` +
		`"minimumOn":"30m0s","maximumOn":"2h0m0s","minimumOff":"1h0m0s","maximumStartsPerDay":4}`
	want := Entry{
		Capability: "on", Default: json.RawMessage("false"), Resolve: NamePreferOff, External: NameOverride,
		OverrideHold: "1h", MinimumOn: Duration(30 * time.Minute), MaximumOn: Duration(2 * time.Hour),
		MinimumOff: Duration(time.Hour), MaximumStartsPerDay: 4,
	}
	var got Entry
	if err := json.Unmarshal([]byte(written), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read %+v, want %+v", got, want)
	}
	again, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != written {
		t.Errorf("wrote %s, want %s", again, written)
	}
}

func TestEntryLeavesOutWhatIsntSet(t *testing.T) {
	raw, err := json.Marshal(Entry{Capability: "on", Default: json.RawMessage("true"), Resolve: NamePreferOn, External: NameRevert})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"capability":"on","default":true,"resolve":"PreferOn","external":"Revert"}`
	if string(raw) != want {
		t.Errorf("got %s, want %s", raw, want)
	}
}

func TestEntryEqual(t *testing.T) {
	entry := Entry{Capability: "on", Default: json.RawMessage(`false`), Resolve: NamePreferOn}
	same := entry
	same.Default = json.RawMessage(`false`)
	otherDefault := entry
	otherDefault.Default = json.RawMessage(`true`)
	otherResolve := entry
	otherResolve.Resolve = NamePreferOff
	for _, c := range []struct {
		name string
		a, b Entry
		want bool
	}{
		{"same", entry, same, true},
		{"nil and empty Default", Entry{}, Entry{Default: json.RawMessage{}}, true},
		{"different Default", entry, otherDefault, false},
		{"different field", entry, otherResolve, false},
	} {
		if got := c.a.Equal(c.b); got != c.want {
			t.Errorf("%s: Equal = %v, want %v", c.name, got, c.want)
		}
	}
}
