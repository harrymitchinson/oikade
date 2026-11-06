package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
		c, welcome, err := wire.Dial(ctx, socket, nil)
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
