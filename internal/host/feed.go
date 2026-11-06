package host

import (
	"sync"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// feedBuffer bounds the events waiting for one tail. One that falls this far behind is cut off.
const feedBuffer = 1024

// feed passes events on to whoever's tailing. It never waits for them.
type feed struct {
	mu   sync.Mutex
	subs map[chan api.Event]bool
}

func newFeed() *feed { return &feed{subs: map[chan api.Event]bool{}} }

func (f *feed) publish(e api.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub := range f.subs {
		select {
		case sub <- e:
		default:
			delete(f.subs, sub)
			close(sub)
		}
	}
}

// subscribe returns the events from now on, closed if the reader falls behind, and how to stop.
func (f *feed) subscribe() (<-chan api.Event, func()) {
	sub := make(chan api.Event, feedBuffer)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[sub] = true
	return sub, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.subs[sub] {
			delete(f.subs, sub)
			close(sub)
		}
	}
}
