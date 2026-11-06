package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/host/wire"
)

// A process that couldn't read these at its start would die at every start, so each run goes in a
// message of its own: together they're far more than one message can hold.
func TestUnfinishedRunsFarLargerThanAMessageReachTheNextProcess(t *testing.T) {
	socket, _ := startHost(t, tempDir(t))
	const runs, size = 6, 4 << 20

	first, _ := dial(t, socket)
	entry := bytes.Repeat([]byte("x"), size)
	for i := range runs {
		if _, err := first.Journal(t.Context(), fmt.Sprintf("flow/%d", i), entry); err != nil {
			t.Fatal(err)
		}
	}
	first.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		c, welcome, err := wire.Dial(ctx, socket, nil, nil)
		if errors.Is(err, wire.ErrRejected) && ctx.Err() == nil {
			continue // The first session is still ending.
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		if len(welcome.Unfinished) != runs {
			t.Fatalf("got %d runs", len(welcome.Unfinished))
		}
		for _, r := range welcome.Unfinished {
			if len(r.Entries) != 1 || len(r.Entries[0]) != size {
				t.Fatalf("run %s came back wrong", r.ID)
			}
		}
		return
	}
}

// With no process running, nothing else ends the run a pause should have, and nothing can say
// whether the rule is a Safety one.
func TestAPauseWithNoProcessEndsTheFlowsRunOnceAsked(t *testing.T) {
	dir := tempDir(t)
	socket, _ := startHost(t, dir)
	first, _ := dial(t, socket)
	if _, err := first.Journal(t.Context(), "flow/1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Journal(t.Context(), "other/1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	first.Close()
	<-first.Done()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local := api.Local{DataDir: dir}
	for {
		resp, err := api.Call(ctx, local, api.Request{Type: api.TypeStatus})
		if err != nil {
			t.Fatal(err)
		}
		if !resp.Status.Process {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := api.Call(ctx, local, api.Request{Type: api.TypePause, Rule: "flow"})
	if err != nil || !resp.NeedsAllowSafety {
		t.Fatalf("paused something it couldn't check: %+v, %v", resp, err)
	}
	resp, err = api.Call(ctx, local, api.Request{Type: api.TypePause, Rule: "flow", AllowSafety: true})
	if err != nil || resp.Error != "" {
		t.Fatalf("got %+v, %v", resp, err)
	}

	_, welcome := dial(t, socket)
	if len(welcome.Unfinished) != 1 || welcome.Unfinished[0].ID != "other/1" {
		t.Errorf("got runs %+v", welcome.Unfinished)
	}
	if len(welcome.Paused) != 1 || welcome.Paused[0] != "flow" {
		t.Errorf("got paused %v", welcome.Paused)
	}
}

func TestAResumeWithNoProcessLeavesPausedTimesBehind(t *testing.T) {
	dir := tempDir(t)
	socket, _ := startHost(t, dir)
	first, _ := dial(t, socket)
	before := time.Now().Add(-24 * time.Hour)
	if err := first.SaveFire(t.Context(), "morning", before); err != nil {
		t.Fatal(err)
	}
	first.Close()
	<-first.Done()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local := api.Local{DataDir: dir}
	for {
		resp, err := api.Call(ctx, local, api.Request{Type: api.TypeStatus})
		if err != nil {
			t.Fatal(err)
		}
		if !resp.Status.Process {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, kind := range []string{api.TypePause, api.TypeResume} {
		resp, err := api.Call(ctx, local, api.Request{Type: kind, Rule: "morning", AllowSafety: true})
		if err != nil || resp.Error != "" {
			t.Fatalf("%s: got %+v, %v", kind, resp, err)
		}
	}
	_, welcome := dial(t, socket, "morning")
	if got := welcome.Fires["morning"]; !got.After(time.Now().Add(-time.Minute)) {
		t.Errorf("the next process would catch up from %s", got)
	}
	if _, untimed := welcome.Fires["other"]; untimed {
		t.Error("a rule with no timers got one")
	}
}
