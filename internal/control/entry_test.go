package control

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

// The host keeps entries in its registry and reads them from --simulate files, so a Device's JSON
// and an Entry's have to stay one format.
func TestEntryReadsADevicesJSON(t *testing.T) {
	starts := int32(4)
	api := homev1alpha1.CapabilityControl{
		Capability: "switch.on", Default: apiextensionsv1.JSON{Raw: []byte("false")},
		Resolve: homev1alpha1.ControlResolvePreferOff, External: homev1alpha1.ControlExternalOverride,
		OverrideHold: "1h", MinimumOn: &metav1.Duration{Duration: 30 * time.Minute},
		MaximumOn: &metav1.Duration{Duration: 2 * time.Hour}, MinimumOff: &metav1.Duration{Duration: time.Hour},
		MaximumStartsPerDay: &starts,
	}
	raw, err := json.Marshal(api)
	if err != nil {
		t.Fatal(err)
	}
	var got Entry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if want := FromAPI(api); !reflect.DeepEqual(got, want) {
		t.Errorf("read %+v, FromAPI gave %+v", got, want)
	}
	again, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back homev1alpha1.CapabilityControl
	if err := json.Unmarshal(again, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(FromAPI(back), got) {
		t.Errorf("wrote %s, which reads back as %+v", again, FromAPI(back))
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
