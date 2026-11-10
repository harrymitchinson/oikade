package im

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/contain"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// subscription sends a client the attributes it subscribed to when they change, at most every
// min, and an empty report when nothing has changed for max (Core spec, 8.5).
type subscription struct {
	id       uint32
	session  *transport.Session
	paths    []AttributePath
	min, max time.Duration
	filtered bool // fabric filtered

	mu     sync.Mutex
	dirty  map[Path]bool
	wake   chan struct{} // cap 1
	cancel context.CancelCauseFunc
}

// maxIntervalTarget is the longest max interval, in seconds, the server chooses itself. A
// subscriber that asks for longer is granted this, so one that misses the host's restart notices
// it within about this long and subscribes again, instead of after the 10 minutes Apple's home hub
// asks for (OIK-209).
const maxIntervalTarget uint16 = 60

// grantMaxInterval chooses the max interval for a subscriber's floor and ceiling, in seconds:
// maxIntervalTarget or the ceiling, whichever is shorter, but never below the floor, and never 0,
// which would report without pause. For a floor at most the ceiling, which subscribe checks, that
// is within the spec's bounds for a publisher: MinIntervalFloor ≤ MaxInterval ≤ max(60 min,
// MaxIntervalCeiling) (Core spec, 8.5).
func grantMaxInterval(floor, ceiling uint16) uint16 {
	return max(min(ceiling, maxIntervalTarget), floor, 1)
}

// errReplaced ends a subscription whose client subscribed again without keeping its others.
var errReplaced = errors.New("replaced by a new subscription")

func (s *Server) subscribe(ctx context.Context, ex *transport.Exchange, b []byte) error {
	req, err := decodeReadRequest(b, true)
	if err != nil {
		return fail(ctx, ex, StatusInvalidAction, err)
	}
	if req.minFloor > req.maxCeiling {
		return fail(ctx, ex, StatusInvalidAction, fmt.Errorf("min interval %d s over max %d s", req.minFloor, req.maxCeiling))
	}
	session := ex.Session()
	replaced := 0
	if !req.keepSubscriptions {
		replaced = s.cancelSubscriptions(session)
	}

	maxInterval := grantMaxInterval(req.minFloor, req.maxCeiling)
	sub := &subscription{
		id: s.newSubscriptionID(), session: session, paths: req.attributes, filtered: req.fabricFiltered,
		min: time.Duration(req.minFloor) * time.Second, max: time.Duration(maxInterval) * time.Second,
		dirty: map[Path]bool{}, wake: make(chan struct{}, 1),
	}
	// Take a slot before answering, so the cap holds and the client never hears of a
	// subscription the server doesn't have yet.
	runCtx, stop, err := s.take(ctx, sub)
	if err != nil {
		return fail(ctx, ex, StatusResourceExhausted, err)
	}

	// The priming report: everything subscribed to, then the Subscribe Response.
	err = s.prime(ctx, ex, sub, req.filters)
	if err == nil {
		var w tlv.Writer
		w.StartStructure(tlv.Anonymous)
		w.PutUint(tlv.Context(0), uint64(sub.id))
		w.PutUint(tlv.Context(2), uint64(maxInterval))
		w.PutUint(tagRevision, revision)
		w.End()
		err = ex.Send(ctx, imMessage(OpSubscribeResponse, w.Bytes()))
	}
	if err != nil {
		stop()
		s.endSubscription(sub)
		return err
	}
	// Info, with min, so the log shows how long a change may wait for its report (OIK-208).
	// Durations as strings, which the host's JSON log would otherwise print in nanoseconds.
	s.Log.Info("Started subscription", "subscription", sub.id, "peer", session.Peer(), "node", fmt.Sprintf("%016X", session.PeerNodeID()),
		"min", sub.min.String(), "max", sub.max.String(), "replaced", replaced)
	s.save(sub)
	go s.serveSubscription(runCtx, sub, stop)
	return nil
}

// take gives sub a slot among the server's subscriptions and starts it hearing changes. It returns
// the subscription's context, and stop, which ends both.
func (s *Server) take(ctx context.Context, sub *subscription) (context.Context, func(), error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	sub.cancel = cancel
	s.mu.Lock()
	if len(s.subs) >= maxSubscriptions {
		s.mu.Unlock()
		cancel(nil)
		return nil, nil, errors.New("too many subscriptions")
	}
	if s.subs == nil {
		s.subs = map[uint32]*subscription{}
	}
	s.subs[sub.id] = sub
	s.mu.Unlock()
	unlisten := s.Node.listen(sub.changed)
	return runCtx, func() { unlisten(); cancel(nil) }, nil
}

// prime sends the priming report on ex: everything sub covers.
func (s *Server) prime(ctx context.Context, ex *transport.Exchange, sub *subscription, filters []dataVersionFilter) error {
	var reports [][]byte
	rq := newRequest(sub.session, sub.filtered)
	s.Node.View(func() { reports = s.reports(rq, sub.paths, filters) })
	err := sendReports(ctx, ex, reports, &sub.id, false)
	if err == nil && s.Observer != nil {
		s.Observer.Reported()
	}
	return err
}

// serveSubscription reports on sub until it ends, then lets its slot go.
func (s *Server) serveSubscription(ctx context.Context, sub *subscription, stop func()) {
	session := sub.session
	defer stop()
	defer s.endSubscription(sub)
	// A panic ends the subscription, and its session, so the controller starts again.
	defer contain.Recover(s.Log, contain.Subscription, s.panicked, func() {
		if s.EndSession != nil {
			s.EndSession(session)
		}
	})
	err := s.runSubscription(ctx, sub)
	if cause := context.Cause(ctx); cause != nil && errors.Is(err, context.Canceled) {
		err = cause // a report cut short says context canceled, not why
	}
	// A client that has forgotten the subscription won't take it back after a restart either.
	if errors.Is(err, StatusInvalidSubscription) {
		s.forget(sub.session.FabricIndex(), sub.id)
	}
	s.Log.Info("Ended subscription", "subscription", sub.id, "peer", session.Peer(), "node", fmt.Sprintf("%016X", session.PeerNodeID()),
		"reason", err.Error())
}

// mark marks the changes the subscription covers, and says whether there were any. It runs on a
// handler's goroutine, so its lock goes with defer.
func (sub *subscription) mark(changes []Path) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	marked := false
	for _, c := range changes {
		for _, p := range sub.paths {
			if p.matches(c) {
				sub.dirty[c] = true
				marked = true
				break
			}
		}
	}
	return marked
}

// changed is the Node's listener: it marks what the subscription covers.
func (sub *subscription) changed(changes []Path) {
	if sub.mark(changes) {
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

// runSubscription sends reports until the client stops answering, the session closes or ctx ends.
func (s *Server) runSubscription(ctx context.Context, sub *subscription) error {
	last := time.Now()
	timer := time.NewTimer(sub.max)
	defer timer.Stop()
	for {
		sub.mu.Lock()
		dirty := len(sub.dirty) > 0
		sub.mu.Unlock()
		next := last.Add(sub.max)
		if dirty {
			next = last.Add(sub.min)
		}
		timer.Reset(time.Until(next))
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-sub.session.Done():
			return transport.ErrClosed
		case <-sub.wake:
			continue // recompute when to report
		case <-timer.C:
		}

		sub.mu.Lock()
		changed := sub.dirty
		sub.dirty = map[Path]bool{}
		sub.mu.Unlock()
		var reports [][]byte
		rq := newRequest(sub.session, sub.filtered)
		s.Node.View(func() {
			for p := range changed {
				if s.check(Concrete(p)) == StatusSuccess && s.canRead(rq, p) {
					reports = append(reports, dataReports(rq, s.Node.Endpoint(p.Endpoint).Cluster(p.Cluster), p)...)
				}
			}
		})
		if err := s.report(ctx, sub, reports); err != nil {
			return err
		}
		last = time.Now()
	}
}

// report sends one report on a new exchange. No reports is a keep-alive.
func (s *Server) report(ctx context.Context, sub *subscription, reports [][]byte) error {
	ex, err := sub.session.NewExchange()
	if err != nil {
		return err
	}
	defer ex.Close()
	if err := sendReports(ctx, ex, reports, &sub.id, false); err != nil {
		return err
	}
	if s.Observer != nil {
		s.Observer.Reported()
	}
	return nil
}

func (s *Server) endSubscription(sub *subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs[sub.id] == sub {
		delete(s.subs, sub.id)
	}
}

// cancelSubscriptions ends the subscriptions of the client on session, for a client that asked
// for a fresh start: those on the session, and those on its other sessions from the same node,
// and forgets their saved records. It returns how many it ended.
func (s *Server) cancelSubscriptions(session *transport.Session) int {
	s.mu.Lock()
	node := session.PeerNodeID()
	var ended []*subscription
	for id, sub := range s.subs {
		if sub.session == session || (node != 0 && sub.session.PeerNodeID() == node) {
			sub.cancel(errReplaced)
			delete(s.subs, id)
			ended = append(ended, sub)
		}
	}
	// A saved subscription of the same client, waiting to be resumed, goes too.
	var waiting []SavedSubscription
	for _, rec := range s.reserved {
		if node != 0 && rec.PeerNode == node && rec.FabricIndex == session.FabricIndex() {
			waiting = append(waiting, rec)
		}
	}
	s.mu.Unlock()
	for _, sub := range ended {
		s.forget(sub.session.FabricIndex(), sub.id)
	}
	for _, rec := range waiting {
		s.forget(rec.FabricIndex, rec.ID)
	}
	return len(ended)
}

// Subscriptions returns how many subscriptions are running.
func (s *Server) Subscriptions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}
