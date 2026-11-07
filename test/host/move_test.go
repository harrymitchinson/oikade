package host

import (
	"testing"
	"time"
)

// A rule that names a device keeps hearing it after the device moves room (OIK-216): a moment on
// its handle fires on its next change, and a hold reading it by name decides at once, not on its
// look a minute on.
func TestRulesThatNameADeviceHearItAfterItMoves(t *testing.T) {
	t.Parallel()
	for _, plugin := range []bool{false, true} {
		t.Run(map[bool]string{false: "simulated", true: "plugin"}[plugin], func(t *testing.T) {
			t.Parallel()
			if !plugin {
				r := start(t)
				r.sync()
				r.stdout(nil, "devices", "room", "probe", "landing")
				r.stdout(nil, "devices", "room", "bathroom-sensor", "landing")
				// sync waits on the probe moment, which names the moved probe.
				r.sync()
				r.change("bathroom-sensor", "humidity", 80.0)
				r.wroteWithin(5*time.Second, "fan", true)
				return
			}
			r := startPlugins(t)
			r.untilValue("quiet", false)
			r.stdout(nil, "devices", "room", "quiet", "landing")
			r.setting("quiet", "true")
			r.wroteWithin(5*time.Second, "quiet-heard", true)
			r.wroteWithin(5*time.Second, "quiet-held", true)
		})
	}
}

// wroteWithin waits a short while for a write, much less than a hold's look a minute on.
func (r *rig) wroteWithin(d time.Duration, device string, value any) {
	r.t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r.count(wrote, "device", device, "value", value) > 0 {
			return
		}
	}
	r.t.Fatalf("no write of %s = %v within %s. The log:\n%s", device, value, d, r.dump())
}
