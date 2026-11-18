package overkiz

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

const (
	towel     = "magellan.40000001"
	towelID   = 40000001.0
	towelSays = `{"on":true,"setpoint":15,"temperature":21.94}`
)

// newMagellanCloud is an account whose devices are only on Magellan: Overkiz has never heard of it.
func newMagellanCloud(t *testing.T) *fakeHub {
	t.Helper()
	cloud := newFakeCloud(t).withMagellan()
	cloud.set(func(h *fakeHub) { h.noOverkizAccount = true })
	return cloud
}

// waitWrites waits for n writes to Magellan, and returns them as capability=value.
func waitWrites(b *bench, n int) []string {
	b.t.Helper()
	b.eventually(fmt.Sprintf("%d writes", n), func() bool { return len(b.hub.magellanWrites()) >= n })
	writes := b.hub.magellanWrites()
	got := make([]string, 0, len(writes))
	for _, w := range writes {
		if w["deviceId"] != towelID {
			b.t.Errorf("wrote to device %v (%T), not %v", w["deviceId"], w["deviceId"], towelID)
		}
		got = append(got, fmt.Sprintf("%v=%v", w["capabilityId"], w["value"]))
	}
	return got
}

func TestAMagellanAccountServesItsTowelRadiator(t *testing.T) {
	cloud := newMagellanCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitState(towel, towelSays)
	b.waitAvailability(towel, "online")
	want := []contractv1.DeviceReport{{
		ID: towel, Found: true, Manufacturer: "Thermor", Model: "RIVA 5 0750W BLC BRI", Capabilities: []string{"on", "setpoint", "temperature"},
	}}
	if got := b.devices().Devices; !reflect.DeepEqual(got, want) {
		t.Errorf("devices:\n got %+v\nwant %+v", got, want)
	}
	// It's named for its zone, and the water heater, a model the plugin doesn't serve, isn't listed.
	// The list waits for Overkiz to say it has nothing.
	b.eventually("the discovered list", func() bool { return b.devices().Discovered != nil })
	if got := b.devices().Discovered; len(got) != 1 || got[0].ID != towel || got[0].Name != "Salle de bain" {
		t.Errorf("discovered %+v", got)
	}
	if n := b.logs.count("Skipped Magellan device of a kind the plugin doesn't serve"); n != 1 || !strings.Contains(b.logs.String(), `"model":"1658"`) {
		t.Errorf("the water heater logged %d times:\n%s", n, b.logs.String())
	}
	// Overkiz having no account isn't a problem.
	b.eventually("Overkiz found absent", func() bool { return b.logs.count("Found no Overkiz account for the login") == 1 })
	if e := b.status().Error; e != "" {
		t.Errorf("status error %q", e)
	}

	// A setpoint in its programme is the override it takes, and the setup is read again once it's done.
	b.set(towel, `{"setpoint":16}`)
	b.waitState(towel, `{"on":true,"setpoint":16,"temperature":21.94}`)
	if got := waitWrites(b, 1); !reflect.DeepEqual(got, []string{"40=16"}) {
		t.Errorf("wrote %v", got)
	}
	// Off is its mode, and on from off is manual: the mode, then its programme off.
	b.set(towel, `{"on":false}`)
	b.waitState(towel, `{"on":false,"setpoint":16,"temperature":21.94}`)
	b.set(towel, `{"on":true}`)
	b.waitState(towel, `{"on":true,"setpoint":16,"temperature":21.94}`)
	if got := waitWrites(b, 4); !reflect.DeepEqual(got, []string{"40=16", "7=0", "7=4", "184=0"}) {
		t.Errorf("wrote %v", got)
	}
	// On while it's heating leaves its mode be, and a setpoint outside its range isn't sent.
	b.set(towel, `{"on":true}`)
	b.set(towel, `{"setpoint":29}`)
	b.set(towel, `{"setpoint":17.5}`)
	if got := waitWrites(b, 5); !reflect.DeepEqual(got, []string{"40=16", "7=0", "7=4", "184=0", "40=17.5"}) {
		t.Errorf("wrote %v", got)
	}

	// What the app changes is read within the read interval.
	cloud.magellanDevice(towelID, func(dev map[string]any) { dev["isAvailable"] = false })
	b.waitAvailability(towel, "offline")
}

func TestAnAccountOnBothCloudsServesBothAfterOneLogin(t *testing.T) {
	cloud := newFakeCloud(t).withMagellan()
	b := startCloud(t, cloud, password, cloudFast, "io.55555555")
	b.waitState(radiator, `{"on":true,"setpoint":21,"temperature":18.5}`)
	b.waitState(towel, towelSays)
	b.eventually("both, and the declared device not found", func() bool {
		d := b.devices().Devices
		return len(d) == 3 && d[0].ID == "io.55555555" && !d[0].Found && d[1].ID == radiator && d[1].Found && d[2].ID == towel && d[2].Found
	})
	b.eventually("both discovered", func() bool { return len(b.devices().Discovered) == 2 })
	if n := cloud.count("/token"); n != 1 {
		t.Errorf("%d logins at Atlantic, want 1", n)
	}

	// Magellan down leaves the Overkiz radiator online, and says so.
	cloud.set(func(h *fakeHub) { h.magellanDown = true })
	b.waitAvailability(towel, "offline")
	b.eventually("the problem", func() bool { return strings.Contains(b.status().Error, "could not reach Thermor's Cozytouch cloud") })
	if got := b.topic(root.Availability(radiator)); got != "online" {
		t.Errorf("the Overkiz radiator is %s", got)
	}
	// While a cloud can't see its app, neither does the plugin, so nothing reads as removed.
	if raw := b.topic(root.Devices()); strings.Contains(raw, `"discovered"`) {
		var d contractv1.Devices
		_ = json.Unmarshal([]byte(raw), &d)
		if d.Discovered != nil {
			t.Errorf("discovered %+v while Magellan is down", d.Discovered)
		}
	}
	cloud.set(func(h *fakeHub) { h.magellanDown = false })
	b.waitAvailability(towel, "online")
	b.eventually("no problem", func() bool { return b.status().Error == "" })
}

func TestARefusedPasswordIsOneProblemAndOneLogin(t *testing.T) {
	cloud := newMagellanCloud(t)
	b := startCloud(t, cloud, "wrong", cloudFast, towel)
	b.eventually("the refusal", func() bool { return strings.Contains(b.status().Error, "refused the login") })
	if e := b.status().Error; strings.Count(e, "refused the login") != 1 {
		t.Errorf("status error %q", e)
	}
	time.Sleep(10 * cloudFast.secret)
	if n := cloud.count("/token"); n != 1 {
		t.Errorf("%d logins at Atlantic with a refused password, want 1", n)
	}
	if d := b.devices().Devices; len(d) != 1 || d[0].Found {
		t.Errorf("devices %+v", d)
	}
	b.secret(password)
	b.waitState(towel, towelSays)
	b.eventually("no problem", func() bool { return b.status().Error == "" })
}

func TestAnEndedTokenSignsInAgainWithTheTowelRadiatorOnline(t *testing.T) {
	cloud := newMagellanCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(towel, "online")
	// A session that lasted, so it's logged in again at once.
	time.Sleep(2 * cloudFast.retryMax)
	logins := cloud.count("/token")
	cloud.expireTokens()
	b.eventually("a new login", func() bool { return cloud.count("/token") > logins })
	b.eventually("the log", func() bool { return b.logs.count("Magellan ended the plugin's session, so it's logging in again") > 0 })
	cloud.magellanDevice(towelID, func(dev map[string]any) {
		for _, c := range dev["capabilities"].([]any) {
			if cp := c.(map[string]any); cp["capabilityId"] == 117.0 {
				cp["value"] = "22.5"
			}
		}
	})
	b.waitState(towel, `{"on":true,"setpoint":15,"temperature":22.5}`)
	if got := b.topic(root.Availability(towel)); got != "online" {
		t.Errorf("the towel radiator went %s", got)
	}
	if e := b.status().Error; e != "" {
		t.Errorf("status error %q", e)
	}
}

func TestOverkizHavingNoAccountIsLoggedOnce(t *testing.T) {
	cloud := newMagellanCloud(t)
	// Overkiz is asked again every refused, hourly in the plugin.
	timings := cloudFast
	timings.refused = 20 * time.Millisecond
	b := startCloud(t, cloud, password, timings)
	b.waitState(towel, towelSays)
	b.eventually("three Overkiz logins", func() bool { return cloud.count("/login") >= 3 })
	if n := b.logs.count("Found no Overkiz account for the login"); n != 1 {
		t.Errorf("logged %d times at Info, not once:\n%s", n, b.logs.String())
	}
	if e := b.status().Error; e != "" {
		t.Errorf("status error %q", e)
	}
}

func TestAnOverkizAccountWithNoMagellanSetupIsntAProblem(t *testing.T) {
	cloud := newFakeCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(radiator, "online")
	b.eventually("Magellan found absent", func() bool { return b.logs.count("Found no Magellan account for the login") == 1 })
	if e := b.status().Error; e != "" {
		t.Errorf("status error %q", e)
	}
	if d := b.devices().Discovered; len(d) != 1 || d[0].ID != radiator {
		t.Errorf("discovered %+v", d)
	}
}

func TestMagellanTowelRadiatorValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps capabilities
		want contractv1.State
	}{
		{"heating", capabilities{7: "4", 40: "19.5", 117: "20.1"}, contractv1.State{"on": true, "setpoint": 19.5, "temperature": 20.1}},
		{"off", capabilities{7: "0", 40: "15"}, contractv1.State{"on": false, "setpoint": 15.0}},
		{"a mode it doesn't know is heating", capabilities{7: "3"}, contractv1.State{"on": true}},
		{"unreadable values are left out", capabilities{40: "", 117: "n/a"}, contractv1.State{}},
	} {
		if got := mTowelDryer.values(tc.caps); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := mTowelDryer.write("setpoint", 6.5, capabilities{160: "7", 161: "28"}); err == nil {
		t.Error("6.5 was taken below a minimum of 7")
	}
	if w, err := mTowelDryer.write("setpoint", 40.0, capabilities{}); err != nil || len(w) != 1 {
		t.Errorf("without a range, 40 wrote %v, %v", w, err)
	}
	if _, err := mTowelDryer.write("temperature", 20.0, capabilities{}); err == nil {
		t.Error("temperature was written")
	}
}

// TestAMagellanSetupThatIsntJSONIsAnError: a setup Magellan answers with that isn't what was asked
// for fails the read rather than reading as empty, and IDs and values come as numbers or strings.
func TestAMagellanSetupThatIsntJSONIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		devices int
		fails   bool
	}{
		"html":                   {body: "<html>maintenance</html>", fails: true},
		"truncated":              {body: `[{"devices":[{"deviceId":1`, fails: true},
		"capabilities an object": {body: `[{"devices":[{"deviceId":1,"capabilities":{}}]}]`, fails: true},
		"one setup":              {body: `{"devices":[{"deviceId":"1","capabilities":[{"capabilityId":"7","value":4}]}]}`, devices: 1},
		"no setup":               {body: `null`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := newMagellanClient(server.Client(), server.URL, "token")
			setups, err := c.setup(t.Context())
			switch {
			case tc.fails && err == nil:
				t.Fatalf("got %+v, no error", setups)
			case !tc.fails && err != nil:
				t.Fatal(err)
			case tc.devices > 0 && (len(setups) != 1 || len(setups[0].Devices) != tc.devices ||
				setups[0].Devices[0].Capabilities[0].ID != "7" || setups[0].Devices[0].Capabilities[0].Value != "4"):
				t.Errorf("read %+v", setups)
			}
		})
	}
}

func TestAQuickOffThenOnWritesBoth(t *testing.T) {
	cloud := newMagellanCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(towel, "online")
	b.set(towel, `{"on":false}`)
	b.set(towel, `{"on":true}`)
	if got := waitWrites(b, 3); !reflect.DeepEqual(got, []string{"7=0", "7=4", "184=0"}) {
		t.Errorf("wrote %v", got)
	}
}

// A token that ended while Magellan was down is refused as the next session starts: that's a new
// token, not a refused password.
func TestATokenThatEndedMeanwhileIsntARefusedPassword(t *testing.T) {
	cloud := newMagellanCloud(t)
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(towel, "online")
	cloud.set(func(h *fakeHub) { h.magellanDown = true })
	b.waitAvailability(towel, "offline")
	cloud.expireTokens()
	cloud.set(func(h *fakeHub) { h.magellanDown = false })
	b.waitAvailability(towel, "online")
	if n := b.logs.count("Magellan refused the credential"); n != 0 {
		t.Errorf("a refused credential logged %d times:\n%s", n, b.logs.String())
	}
}

// A 404 that isn't Magellan's own, such as a proxy's, is an error, not an account with no setup.
func TestAProxysNotFoundIsntNoAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>Not Found</html>"))
	}))
	defer server.Close()
	_, err := newMagellanClient(server.Client(), server.URL, "token").setup(t.Context())
	if err == nil || classify(err) == absent {
		t.Errorf("got %v, %v", err, classify(err))
	}
}

// A setpoint moved and moved straight back ends where it was moved last: Magellan refuses a write to
// a device while its last runs, so each waits for the one before to end, and a newer write replaces
// one still waiting (OIK-339).
func TestAQuickSecondWriteWaitsForTheFirst(t *testing.T) {
	cloud := newMagellanCloud(t)
	cloud.set(func(h *fakeHub) { h.magellanRuns = 3 })
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(towel, "online")
	b.set(towel, `{"setpoint":17.5}`)
	b.set(towel, `{"setpoint":16}`)
	b.waitState(towel, `{"on":true,"setpoint":16,"temperature":21.94}`)
	if got := waitWrites(b, 2); !reflect.DeepEqual(got, []string{"40=17.5", "40=16"}) {
		t.Errorf("wrote %v", got)
	}

	b.set(towel, `{"setpoint":17.5}`)
	b.set(towel, `{"setpoint":16}`)
	b.set(towel, `{"setpoint":18}`)
	b.waitState(towel, `{"on":true,"setpoint":18,"temperature":21.94}`)
	if got := waitWrites(b, 4); !reflect.DeepEqual(got, []string{"40=17.5", "40=16", "40=17.5", "40=18"}) {
		t.Errorf("wrote %v, want the waiting 16 replaced by 18", got)
	}
	if n := cloud.magellanRefused(); n != 0 {
		t.Errorf("Magellan refused %d writes", n)
	}
	if b.logs.count("Magellan execution failed") != 0 {
		t.Errorf("an execution failed:\n%s", b.logs.String())
	}
}

// A write Magellan refuses anyway, as while the Cozytouch app's own write runs, is logged with the
// device and what it set.
func TestARefusedMagellanWriteSaysWhatItWas(t *testing.T) {
	cloud := newMagellanCloud(t)
	cloud.set(func(h *fakeHub) {
		h.magellanRuns = 1000
		h.executing = map[string]*fakeExecution{"1651827": {device: fmt.Sprint(towelID)}}
	})
	b := startCloud(t, cloud, password, cloudFast)
	b.waitAvailability(towel, "online")
	b.set(towel, `{"setpoint":17.5}`)
	b.eventually("the refusal logged", func() bool { return b.logs.count("Magellan execution failed") == 1 })
	for _, want := range []string{`"device":"magellan.40000001"`, `"capability":"setpoint"`, `"value":"17.5"`} {
		if !strings.Contains(b.logs.String(), want) {
			t.Errorf("the refusal doesn't say %s:\n%s", want, b.logs.String())
		}
	}
}
