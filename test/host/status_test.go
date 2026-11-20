package host

import (
	"encoding/json"
	"errors"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// status --json is one object, with the same keys, whatever narrows it. A device that goes offline
// says since when, and fails --check only when it's named.
func TestStatusNarrowsAndChecks(t *testing.T) {
	t.Parallel()
	r := start(t)
	r.sync()
	keys := func(args ...string) string {
		var st map[string]json.RawMessage
		out := r.stdout(nil, append([]string{"status", "--json"}, args...)...)
		if err := json.Unmarshal(out, &st); err != nil {
			t.Fatalf("status --json %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.Join(slices.Sorted(maps.Keys(st)), " ")
	}
	whole := keys()
	if whole != "devices home matter plugins rules" {
		t.Errorf("status --json has %s", whole)
	}
	for _, refs := range [][]string{{"fan"}, {"device/fan/on"}, {"case/fan/on/fan"}, {"fan", "bathroom-sensor"}} {
		if got := keys(refs...); got != whole {
			t.Errorf("status --json %s has %s, not %s", strings.Join(refs, " "), got, whole)
		}
	}
	if st := r.status("fan"); len(st.Devices) != 1 || st.Devices[0].Name != "fan" || len(st.Rules) != 0 {
		t.Errorf("status --json fan: %+v", st)
	}

	exits := func(args ...string) int {
		stderr, err := r.oikade(nil, append([]string{"status", "--check"}, args...)...)
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit):
			if strings.Contains(stderr, "Error:") {
				t.Errorf("status --check %s: %s", strings.Join(args, " "), stderr)
			}
			return exit.ExitCode()
		}
		t.Fatalf("status --check %s: %v", strings.Join(args, " "), err)
		return 0
	}
	if code := exits(); code != 0 {
		t.Errorf("status --check of a home with nothing wrong exits %d", code)
	}
	r.online("fan", false)
	r.until("the fan to be offline", func() bool {
		st := r.status("fan")
		return st.Devices[0].Online != nil && !*st.Devices[0].Online
	})
	if d := r.status("fan").Devices[0]; d.OfflineSince == nil {
		t.Errorf("the fan is offline, but not since a time: %+v", d)
	}
	out := string(r.stdout(nil, "status", "fan"))
	if !strings.HasPrefix(out, "fan") || !strings.Contains(out, ": went offline ") ||
		!strings.Contains(out, "device/fan  went offline") {
		t.Errorf("status fan:\n%s", out)
	}
	for args, want := range map[string]int{"": 0, "fan": 1, "bathroom-sensor": 0} {
		if code := exits(strings.Fields(args)...); code != want {
			t.Errorf("status --check %s with the fan offline exits %d, want %d", args, code, want)
		}
	}
	r.online("fan", true)
	r.until("the fan to be back", func() bool { return r.status("fan").Devices[0].OfflineSince == nil })
}
