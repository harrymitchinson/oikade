package transport

// windowSize is how many counters before the largest one a receiver remembers (Core spec,
// 4.6.5, MSG_COUNTER_WINDOW_SIZE).
const windowSize = 32

// counterVerdict is what a receiver makes of a message counter.
type counterVerdict uint8

const (
	counterNew       counterVerdict = iota // never seen: process it
	counterDuplicate                       // seen, inside the window: acknowledge it again, don't process
	counterTooOld                          // behind the window of a session that can't roll over: drop it
)

// reception is a peer's message reception state (4.6.5.1): the largest counter seen, and a
// bitmap of which of the windowSize counters before it were seen.
type reception struct {
	synced bool
	max    uint32
	seen   uint32 // bit i: max-1-i was seen
	// rollover is true for unencrypted messages, whose counters may wrap and restart. Encrypted
	// unicast counters never roll over, so anything behind the window is old.
	rollover bool
}

// check says whether counter c is new, without recording it.
func (r *reception) check(c uint32) counterVerdict {
	if !r.synced {
		return counterNew // trust the first counter
	}
	if c == r.max {
		return counterDuplicate
	}
	if r.rollover {
		// Counters within the window behind max are duplicates if seen. Anything else, ahead or
		// far behind, is accepted as new and resets the window.
		if d := r.max - c; d <= windowSize {
			if r.seen&(1<<(d-1)) != 0 {
				return counterDuplicate
			}
		}
		return counterNew
	}
	if c > r.max {
		return counterNew
	}
	d := r.max - c
	if d > windowSize {
		return counterTooOld
	}
	if r.seen&(1<<(d-1)) != 0 {
		return counterDuplicate
	}
	return counterNew
}

// accept records counter c, which check said is new.
func (r *reception) accept(c uint32) {
	if !r.synced {
		r.synced, r.max, r.seen = true, c, 0
		return
	}
	if r.rollover {
		if d := r.max - c; d >= 1 && d <= windowSize {
			r.seen |= 1 << (d - 1)
			return
		}
		ahead := c - r.max
		if ahead > 0 && ahead <= windowSize {
			r.shift(ahead)
		} else {
			r.seen = 0 // a jump either way restarts the window
		}
		r.max = c
		return
	}
	if c > r.max {
		ahead := c - r.max
		if ahead > windowSize {
			r.seen = 0
		} else {
			r.shift(ahead)
		}
		r.max = c
		return
	}
	r.seen |= 1 << (r.max - c - 1)
}

// shift moves the window forward by n ≤ windowSize, marking the old max as seen.
func (r *reception) shift(n uint32) {
	if n == windowSize {
		r.seen = 1 << (windowSize - 1)
		return
	}
	r.seen = r.seen<<n | 1<<(n-1)
}
