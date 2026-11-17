package plugin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// huge is a payload far larger than any message a plugin sends.
var huge = "{" + strings.Repeat(`"x":1,`, 200_000) + `"y":`

// rejectedDelta publishes each payload to topic and returns how many rejections each reason gained.
func rejectedDelta(t *testing.T, i *Instance, bus *mqtt.Memory, topic string, payloads ...string) map[string]uint64 {
	t.Helper()
	before := i.Stats().Rejected
	for _, p := range payloads {
		if err := bus.Publish(t.Context(), topic, []byte(p), false); err != nil {
			t.Fatal(err)
		}
	}
	delta := map[string]uint64{}
	for reason, n := range i.Stats().Rejected {
		if d := n - before[reason]; d > 0 {
			delta[reason] = d
		}
	}
	return delta
}

func wantDelta(t *testing.T, what string, got, want map[string]uint64) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: rejected %v, want %v", what, got, want)
	}
}

// TestAMalformedDevicesMessageKeepsTheLastOne: a devices message that isn't the object the
// contract describes is counted as malformed, and the devices stay as the last good one said.
func TestAMalformedDevicesMessageKeepsTheLastOne(t *testing.T) {
	i, bus, rec := setup(t, online())
	payloads := []string{`not json`, ``, `[]`, `{"devices":"1000aa"}`, `{"devices":[{"id":1}]}`, `{"devices":[`, huge}
	wantDelta(t, "devices", rejectedDelta(t, i, bus, root+"/devices", payloads...), map[string]uint64{ReasonMalformed: uint64(len(payloads))})
	if got := rec.discovered[porch]; !got.Found || got.Model != "MINIR4" {
		t.Errorf("porch = %+v, want what the last good devices message said", got)
	}
	if got := i.Announced(); len(got) != 3 {
		t.Errorf("announced %v, want the last good message's three", got)
	}
}

// TestADevicesMessageWithInvalidUTF8 reads as JSON does: each invalid byte is U+FFFD, so a
// capability with one is another capability, and an ID with one is invalid.
func TestADevicesMessageWithInvalidUTF8(t *testing.T) {
	i, bus, rec := setup(t, online())
	payload := `{"devices":[{"id":"1000aa","found":true,"capabilities":["on","on` + "\xff" + `"]},{"id":"1000bb` + "\xfe" + `","found":true}]}`
	wantDelta(t, "devices", rejectedDelta(t, i, bus, root+"/devices", payload), map[string]uint64{ReasonUnknownCapability: 1, ReasonMalformed: 1})
	if got := rec.discovered[porch]; !got.Found || len(got.Capabilities) != 1 || got.Capabilities[0] != capability.On {
		t.Errorf("porch = %+v, want on alone", got)
	}
	if got := rec.discovered[hall]; got.Found {
		t.Errorf("hall = %+v, want not found: its ID was invalid", got)
	}
}

// TestADeviceWithAnInvalidOrRepeatedIDIsDropped: an entry in devices whose ID isn't valid, or
// repeats an earlier one's, is dropped and counted as malformed, as for discovered, and the rest of
// the message is kept. The first entry for an ID wins.
func TestADeviceWithAnInvalidOrRepeatedIDIsDropped(t *testing.T) {
	i, bus, rec := setup(t, online())
	payload := `{"devices":[
		{"id":"","found":true,"capabilities":["on"]},
		{"id":"a/b","found":true,"capabilities":["on"]},
		{"id":"-1000aa","found":true,"capabilities":["on"]},
		{"id":"1000aa","found":true,"model":"first","capabilities":["on"]},
		{"id":"1000aa","found":true,"model":"second","capabilities":["on","temperature"]},
		{"id":"1000bb","found":false,"error":"no LAN key"}
	]}`
	wantDelta(t, "devices", rejectedDelta(t, i, bus, root+"/devices", payload), map[string]uint64{ReasonMalformed: 4})
	if got := rec.discovered[porch]; !got.Found || got.Model != "first" || len(got.Capabilities) != 1 {
		t.Errorf("porch = %+v, want the first entry for 1000aa", got)
	}
	if got := rec.discovered[hall]; got.Found || got.Error != "no LAN key" {
		t.Errorf("hall = %+v, want the entry after the dropped ones", got)
	}
	if got := i.Announced(); len(got) != 2 {
		t.Errorf("announced %v, want 1000aa and 1000bb", got)
	}
}

// TestAMalformedStatusKeepsTheLastOne: a status that isn't the object the contract describes, or
// whose state is neither online nor offline, is counted as malformed, and the plugin stays up.
func TestAMalformedStatusKeepsTheLastOne(t *testing.T) {
	i, bus, _ := setup(t, online())
	payloads := []string{
		`not json`, ``, `[]`, `{"state":"maybe","contract":1}`, `{"contract":1}`, `{"state":"ONLINE","contract":1}`,
		`{"state":1}`, `{"state":"online` + "\xff" + `","contract":1}`, huge,
	}
	wantDelta(t, "status", rejectedDelta(t, i, bus, root+"/status", payloads...), map[string]uint64{ReasonMalformed: uint64(len(payloads))})
	if !i.Stats().Up {
		t.Error("a malformed status took the plugin down")
	}
}

// TestAMalformedSetupStepIsCounted: a setup step that isn't JSON is counted as malformed, whether
// or not a session is running, and a running session ends only for a step of its own.
func TestAMalformedSetupStepIsCounted(t *testing.T) {
	i, bus, _ := setup(t, map[string]string{"status": `{"state":"online","contract":1,"setup":true}`, "devices": devicesJSON})
	payloads := []string{`not json`, ``, `[]`, `{"session":1}`, `{"session":"s","seq":"one"}`, huge}
	wantDelta(t, "no session", rejectedDelta(t, i, bus, root+"/_setup/out", payloads...), map[string]uint64{ReasonMalformed: uint64(len(payloads))})

	steps, err := i.StartSetup(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	wantDelta(t, "a session's", rejectedDelta(t, i, bus, root+"/_setup/out", payloads...), map[string]uint64{ReasonMalformed: uint64(len(payloads))})
	// Another session's step that isn't one of the five is nobody's, and isn't checked.
	wantDelta(t, "another session's", rejectedDelta(t, i, bus, root+"/_setup/out", `{"session":"other","seq":1}`), map[string]uint64{})
	if len(steps) != 0 {
		t.Errorf("the session got %d steps from messages that weren't its", len(steps))
	}
	// Its own step that isn't one of the five ends it.
	wantDelta(t, "a step that isn't one", rejectedDelta(t, i, bus, root+"/_setup/out", `{"session":"s","seq":1}`), map[string]uint64{ReasonMalformed: 1})
	if s, ok := <-steps; !ok || s.Done == nil || !strings.Contains(s.Done.Error, "can't use") {
		t.Errorf("got %+v, %v, want the session ended", s, ok)
	}
}

// TestAMalformedAvailabilityIsCounted: an availability that isn't exactly online or offline is
// counted as malformed, and changes nothing.
func TestAMalformedAvailabilityIsCounted(t *testing.T) {
	i, bus, rec := setup(t, online())
	if err := bus.Publish(t.Context(), root+"/1000aa/availability", []byte("online"), false); err != nil {
		t.Fatal(err)
	}
	payloads := []string{``, `ONLINE`, `"offline"`, `offline `, "off\xffline", `{"state":"offline"}`, huge}
	wantDelta(t, "availability", rejectedDelta(t, i, bus, root+"/1000aa/availability", payloads...), map[string]uint64{ReasonMalformed: uint64(len(payloads))})
	if !rec.available[porch] {
		t.Error("a malformed availability took porch offline")
	}
	// An ID no Device has, the empty one among them, is undeclared.
	wantDelta(t, "undeclared", rejectedDelta(t, i, bus, root+"/1000cc/availability", "offline"), map[string]uint64{ReasonUndeclaredDevice: 1})
	wantDelta(t, "empty ID", rejectedDelta(t, i, bus, root+"//availability", "offline"), map[string]uint64{ReasonUndeclaredDevice: 1})
}

// TestAMalformedStateIsCounted: a state that isn't a JSON object is malformed, and an empty or
// unknown ID is undeclared.
func TestAMalformedStateIsCounted(t *testing.T) {
	i, bus, rec := setup(t, online())
	payloads := []string{``, `[]`, `"on"`, `{"on":`, huge}
	wantDelta(t, "state", rejectedDelta(t, i, bus, root+"/1000aa/state", payloads...), map[string]uint64{ReasonMalformed: uint64(len(payloads))})
	wantDelta(t, "invalid UTF-8", rejectedDelta(t, i, bus, root+"/1000aa/state", `{"on`+"\xff"+`":true}`), map[string]uint64{ReasonUnknownCapability: 1})
	wantDelta(t, "empty ID", rejectedDelta(t, i, bus, root+"//state", `{"on":true}`), map[string]uint64{ReasonUndeclaredDevice: 1})
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("rejected states changed %+v", got)
	}
}

// TestIDsNoDeviceHasAreBounded: a client publishing under the plugin's root with ever new IDs leaves
// no more than maxUnseen of their states, and logs no more than maxUnseen of their rejections, while
// a declared device's state is still kept and replayed.
func TestIDsNoDeviceHasAreBounded(t *testing.T) {
	i, bus, rec := setup(t, map[string]string{"status": `{"state":"online","contract":1}`})
	for n := range 3 * maxUnseen {
		id := fmt.Sprintf("made-up-%d", n)
		for _, m := range []struct{ topic, payload string }{{"/state", `{"on":true}`}, {"/availability", "online"}} {
			if err := bus.Publish(t.Context(), root+"/"+id+m.topic, []byte(m.payload), false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := bus.Publish(t.Context(), root+"/1000aa/state", []byte(`{"on":true}`), false); err != nil {
		t.Fatal(err)
	}
	i.mu.Lock()
	last, logged := len(i.last), len(i.logged)
	i.mu.Unlock()
	if last > maxUnseen+2*3 || logged > maxUnseen+1 {
		t.Errorf("kept %d states and %d logged rejections", last, logged)
	}
	if got := i.Stats().Rejected[ReasonUndeclaredDevice]; got != 2*3*maxUnseen {
		t.Errorf("undeclared_device = %d, want every message counted", got)
	}
	// The devices message replays porch's state, kept although the made-up IDs came first.
	if err := bus.Publish(t.Context(), root+"/devices", []byte(devicesJSON), true); err != nil {
		t.Fatal(err)
	}
	if got := rec.takeChanges(); len(got) != 1 || got[0].Device != porch || got[0].Value != true {
		t.Errorf("replayed %+v, want porch's on", got)
	}
}

// TestARemovedDevicesMessagesAreNotCounted: a plugin that hears devices on the network keeps
// publishing for one you removed, which it can't tell from a new one, so its state and
// availability are dropped without counting as a rejection. Another unknown ID still counts.
func TestARemovedDevicesMessagesAreNotCounted(t *testing.T) {
	i, bus, rec := setup(t, online())
	i.SetDevices(map[string]ident.ID{"1000aa": porch}, map[string]bool{"1000bb": true})
	rec.takeChanges()
	wantDelta(t, "removed state", rejectedDelta(t, i, bus, root+"/1000bb/state", `{"on":true}`, `{"on":false}`), map[string]uint64{})
	wantDelta(t, "removed availability", rejectedDelta(t, i, bus, root+"/1000bb/availability", "online", "offline"), map[string]uint64{})
	if got := rec.takeChanges(); len(got) != 0 {
		t.Errorf("a removed device's state changed %+v", got)
	}
	wantDelta(t, "undeclared", rejectedDelta(t, i, bus, root+"/1000cc/state", `{"on":true}`), map[string]uint64{ReasonUndeclaredDevice: 1})
	// Declared again, as a re-paired device is, its state is used.
	i.SetDevices(map[string]ident.ID{"1000aa": porch, "1000bb": hall}, nil)
	rec.takeChanges()
	publish(t, bus, root+"/1000bb/state", `{"on":true}`)
	if got := rec.takeChanges(); len(got) != 1 || got[0].Device != hall {
		t.Errorf("hall declared again got %+v, want its state", got)
	}
}
