// Package contain keeps a panic in Matter's handling of one message from stopping the host. Matter
// parses what anyone on the network sends, and a bug there should cost that message and its session,
// not the home.
//
// Only work whose state goes with it is contained: a packet, an exchange's handler, a subscription,
// an mDNS query. A panic in a timer, a commissioning hook or a commissioning command would leave the
// node's state half moved, such as a window that never closes, so those still stop the host, which
// starts again clean. Critical marks the commands.
//
// A recover covers only its own goroutine, so each such goroutine defers Recover itself. The locks
// taken on those paths go with defer, so a contained panic can't leave one held, which would turn a
// crash into a hang.
package contain

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Where a panic was contained. Each is a site, the label of oikade_matter_panics_total.
const (
	Receive      = "receive"      // the transport reading a packet
	Handler      = "handler"      // a protocol handler on its exchange
	Subscription = "subscription" // a subscription's reports
	MDNS         = "mdns"         // the mDNS responder answering a packet
)

// Parts is every place, in order.
var Parts = []string{Receive, Handler, Subscription, MDNS}

// Panicked hears each panic contained, with its site.
type Panicked func(site string)

// fatal is a panic that Recover passes on, so it stops the host.
type fatal struct{ value any }

func (f fatal) String() string { return fmt.Sprintf("%v", f.value) }

// Critical makes a panic in what's running fatal, even under a Recover: defer it directly in work
// that a panic would leave half done, as defer contain.Critical().
func Critical() {
	if r := recover(); r != nil {
		if _, ok := r.(fatal); ok {
			panic(r)
		}
		panic(fatal{r})
	}
}

// Recover contains a panic: it logs it with its stack, tells panicked, if set, and then calls after,
// if set, to end what the panic left half done, such as its session. Defer it directly, as
// defer contain.Recover(log, contain.Handler, panicked, nil): recover works only there.
func Recover(log *slog.Logger, site string, panicked Panicked, after func()) {
	r := recover()
	if r == nil {
		return
	}
	if _, ok := r.(fatal); ok {
		panic(r)
	}
	if log == nil {
		log = slog.Default()
	}
	err, ok := r.(error)
	if !ok {
		err = fmt.Errorf("%v", r)
	}
	log.Error("Contained a panic in Matter", "err", err, "site", site, "stack", string(debug.Stack()))
	if panicked != nil {
		panicked(site)
	}
	if after != nil {
		after()
	}
}
