package im

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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
	cancel context.CancelFunc
}

func (s *Server) subscribe(ctx context.Context, ex *transport.Exchange, b []byte) error {
	req, err := decodeReadRequest(b, true)
	if err != nil {
		return fail(ctx, ex, StatusInvalidAction, err)
	}
	if req.minFloor > req.maxCeiling {
		return fail(ctx, ex, StatusInvalidAction, fmt.Errorf("min interval %d s over max %d s", req.minFloor, req.maxCeiling))
	}
	session := ex.Session()
	if !req.keepSubscriptions {
		s.cancelSubscriptions(session)
	}

	maxInterval := max(req.maxCeiling, req.minFloor, 1)
	sub := &subscription{
		id: randomSubscriptionID(), session: session, paths: req.attributes, filtered: req.fabricFiltered,
		min: time.Duration(req.minFloor) * time.Second, max: time.Duration(maxInterval) * time.Second,
		dirty: map[Path]bool{}, wake: make(chan struct{}, 1),
	}
	runCtx, cancel := context.WithCancel(ctx)
	sub.cancel = cancel
	// Take a slot before answering, so the cap holds and the client never hears of a
	// subscription the server doesn't have yet.
	s.mu.Lock()
	if len(s.subs) >= maxSubscriptions {
		s.mu.Unlock()
		cancel()
		return fail(ctx, ex, StatusResourceExhausted, errors.New("too many subscriptions"))
	}
	if s.subs == nil {
		s.subs = map[uint32]*subscription{}
	}
	s.subs[sub.id] = sub
	s.mu.Unlock()
	stop := s.Node.listen(sub.changed)

	// The priming report: everything subscribed to, then the Subscribe Response.
	var reports [][]byte
	rq := newRequest(session, req.fabricFiltered)
	s.Node.View(func() { reports = s.reports(rq, req.attributes, req.filters) })
	err = sendReports(ctx, ex, reports, &sub.id, false)
	if err == nil && s.Observer != nil {
		s.Observer.Reported()
	}
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
		cancel()
		s.endSubscription(sub)
		return err
	}
	go func() {
		defer stop()
		defer s.endSubscription(sub)
		err := s.runSubscription(runCtx, sub)
		s.Log.Debug("Ended subscription", "subscription", sub.id, "peer", session.Peer(), "reason", err.Error())
	}()
	s.Log.Debug("Started subscription", "subscription", sub.id, "peer", session.Peer(), "max", sub.max)
	return nil
}

// changed is the Node's listener: it marks what the subscription covers.
func (sub *subscription) changed(changes []Path) {
	sub.mu.Lock()
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
	sub.mu.Unlock()
	if marked {
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
			return ctx.Err()
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
// for a fresh start: those on the session, and those on its other sessions from the same node.
func (s *Server) cancelSubscriptions(session *transport.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := session.PeerNodeID()
	for id, sub := range s.subs {
		if sub.session == session || (node != 0 && sub.session.PeerNodeID() == node) {
			sub.cancel()
			delete(s.subs, id)
		}
	}
}

// Subscriptions returns how many subscriptions are running.
func (s *Server) Subscriptions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}
