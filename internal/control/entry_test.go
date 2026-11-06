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
	const written = `{"capability":"switch.on","default":false,"resolve":"PreferOff","external":"Override","overrideHold":"1h",` +
		`"minimumOn":"30m0s","maximumOn":"2h0m0s","minimumOff":"1h0m0s","maximumStartsPerDay":4}`
	want := Entry{
		Capability: "switch.on", Default: json.RawMessage("false"), Resolve: NamePreferOff, External: NameOverride,
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
	raw, err := json.Marshal(Entry{Capability: "switch.on", Default: json.RawMessage("true"), Resolve: NamePreferOn, External: NameRevert})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"capability":"switch.on","default":true,"resolve":"PreferOn","external":"Revert"}`
	if string(raw) != want {
		t.Errorf("got %s, want %s", raw, want)
	}
}
