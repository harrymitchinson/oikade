// Package contain keeps a panic in Matter's handling of one message from stopping the host. Matter
// parses what anyone on the network sends, and a bug there should cost that message, not the home.
//
// A recover covers only its own goroutine, so every goroutine that handles a message defers Recover
// itself. Locks taken on the way are let go with defer, so a contained panic can't leave one held,
// which would turn a crash into a hang.
package contain

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
)

// Where a panic was contained. Each is a label of oikade_matter_panics_total.
const (
	Receive       = "receive"       // the transport reading a packet
	Handler       = "handler"       // a protocol handler on its exchange
	Commissioning = "commissioning" // a commissioning hook
	Subscription  = "subscription"  // a subscription's reports
	MDNS          = "mdns"          // the mDNS responder
	Timer         = "timer"         // a timer's callback
)

// Parts is every place, in order.
var Parts = []string{Receive, Handler, Commissioning, Subscription, MDNS, Timer}

var counts = func() map[string]*atomic.Uint64 {
	m := make(map[string]*atomic.Uint64, len(Parts))
	for _, p := range Parts {
		m[p] = new(atomic.Uint64)
	}
	return m
}()

// Recover contains a panic, logs it with its stack, counts it, and then calls after, if set, to end
// what the panic left half done, such as the session it came on. Defer it directly, as
// defer contain.Recover(log, contain.Handler, nil): recover works only there.
func Recover(log *slog.Logger, where string, after func()) {
	r := recover()
	if r == nil {
		return
	}
	if c, ok := counts[where]; ok {
		c.Add(1)
	}
	if log != nil {
		log.Error("Contained a panic in Matter", "err", fmt.Errorf("%v", r), "where", where,
			"stack", string(debug.Stack()))
	}
	if after != nil {
		after()
	}
}

// Panics is how many panics have been contained in where.
func Panics(where string) uint64 {
	if c, ok := counts[where]; ok {
		return c.Load()
	}
	return 0
}
