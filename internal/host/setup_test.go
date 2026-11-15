package host

import (
	"strings"
	"testing"
)

func TestASetupReplyIsTakenOnceAndOnlyForAWaitingStep(t *testing.T) {
	var runs setupRuns
	run := &setupRun{session: "s1", waiting: map[int]bool{}}
	runs.add("relays", run)
	runs.wait(run, 2)
	for _, c := range []struct {
		plugin, session string
		seq             int
		want            string
	}{
		{"other", "s1", 2, "no setup of other"},
		{"relays", "s0", 2, "no setup of relays is running as s0"},
		{"relays", "s1", 1, "step 1 of the setup of relays isn't waiting"},
	} {
		if _, err := runs.take(c.plugin, c.session, c.seq); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v", c, err)
		}
	}
	if got, err := runs.take("relays", "s1", 2); err != nil || got != run {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := runs.take("relays", "s1", 2); err == nil {
		t.Fatal("took a reply twice")
	}
	runs.remove("relays", &setupRun{})
	runs.wait(run, 3)
	if _, err := runs.take("relays", "s1", 3); err != nil {
		t.Fatalf("removing another run removed this one: %v", err)
	}
	runs.remove("relays", run)
	if _, err := runs.take("relays", "s1", 3); err == nil {
		t.Fatal("a removed run took a reply")
	}
}
