package host

import (
	"fmt"
	"testing"
	"time"
)

// quickly is well short of a hold's look a minute on, with room for a busy machine.
const quickly = 20 * time.Second

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
				fan := r.writes("fan", true)
				r.change("bathroom-sensor", "humidity", 80.0)
				fan()
				return
			}
			r := startPlugins(t)
			r.untilValue("quiet", false)
			r.stdout(nil, "devices", "room", "quiet", "landing")
			heard, held := r.writes("quiet-heard", true), r.writes("quiet-held", true)
			r.setting("quiet", "true")
			heard()
			held()
		})
	}
}

// writes counts the writes of a value so far, and returns a wait for one more, quickly.
func (r *rig) writes(device string, value any) (another func()) {
	r.t.Helper()
	before := r.count(wrote, "device", device, "value", value)
	return func() {
		r.t.Helper()
		r.untilWithin(quickly, fmt.Sprintf("a write of %s = %v", device, value), func() bool {
			return r.count(wrote, "device", device, "value", value) > before
		})
	}
}
