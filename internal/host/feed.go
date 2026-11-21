package host

import (
	"sync"

	"oikade.dev/internal/host/api"
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

// backlog is how many events a tail may have waiting while it writes the log's past, which can take
// as long as its caller does to read 64 MiB.
const backlog = 16 * feedBuffer

// queued passes a subscription's events on through a queue of up to limit, so that a reader busy
// with something else for a while isn't cut off for it. Its channel closes when the subscription's
// does, or when the queue is full: the reader fell behind after all.
func queued(in <-chan api.Event, limit int) <-chan api.Event {
	out := make(chan api.Event)
	go func() {
		defer close(out)
		var queue []api.Event
		for {
			var next chan api.Event
			var first api.Event
			if len(queue) > 0 {
				next, first = out, queue[0]
			}
			select {
			case e, open := <-in:
				if !open || len(queue) >= limit {
					return
				}
				queue = append(queue, e)
			case next <- first:
				queue = queue[1:]
			}
		}
	}()
	return out
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
