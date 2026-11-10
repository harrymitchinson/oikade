package im

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// SavedSubscription is a subscription as the server saves it, so that it can resume it under the
// same ID after a restart, as the reference SDK, matter.js and rs-matter do (OIK-209). It holds
// node IDs and paths, never keys.
type SavedSubscription struct {
	FabricIndex    uint8
	ID             uint32
	PeerNode       uint64
	Paths          []AttributePath
	FabricFiltered bool
	Min, Max       uint16 // seconds
	// Peer is the subscriber's last address, for when operational discovery can't find it.
	Peer netip.AddrPort
}

// SubscriptionStore keeps saved subscriptions where they survive a restart. Each write has reached
// disk when it returns.
type SubscriptionStore interface {
	SaveSubscription(SavedSubscription) error
	DeleteSubscription(fabricIndex uint8, id uint32) error
	Subscriptions() ([]SavedSubscription, error)
}

// Errors from Resume.
var (
	// ErrRefused means the subscriber answered the priming report with INVALID_SUBSCRIPTION: it
	// has forgotten the subscription, and will subscribe again itself.
	ErrRefused = errors.New("subscriber refused the resumed subscription")
	// ErrForgotten means the saved subscription was forgotten while it waited to be resumed, as
	// when its subscriber subscribed again first.
	ErrForgotten = errors.New("saved subscription was forgotten")
)

// newSubscriptionID returns an ID that no subscription has, running or saved.
func (s *Server) newSubscriptionID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		id := randomSubscriptionID()
		_, running := s.subs[id]
		if !running && !s.stored[id] {
			return id
		}
	}
}

// LoadSubscriptions returns the saved subscriptions, and keeps their IDs from new ones. Call it at
// the start of each run, before serving. Each is to be resumed with Resume, or given up with Forget.
func (s *Server) LoadSubscriptions() ([]SavedSubscription, error) {
	if s.Saved == nil {
		return nil, nil
	}
	saved, err := s.Saved.Subscriptions()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved, s.stored = map[uint32]SavedSubscription{}, map[uint32]bool{}
	for _, sub := range saved {
		s.reserved[sub.ID] = sub
		s.stored[sub.ID] = true
	}
	return saved, nil
}

// Waiting reports whether a saved subscription is still waiting to be resumed: not resumed yet,
// nor forgotten.
func (s *Server) Waiting(id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.reserved[id]
	return ok
}

// save keeps sub's record, for a CASE session on a fabric. Records of subscriptions that have
// ended go first, so there are never more than the subscriptions the server can hold. It holds
// saveMu, so a forget that comes after it, as when the client replaces the subscription at once,
// deletes what it wrote. A record that can't be written costs only resuming it after a restart.
func (s *Server) save(sub *subscription) {
	if s.Saved == nil || sub.session.AuthMode() != transport.AuthCASE || sub.session.FabricIndex() == 0 {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	saved, err := s.Saved.Subscriptions()
	if err != nil {
		s.Log.Error("Could not read saved subscriptions", "err", err)
		return
	}
	s.mu.Lock()
	var stale []SavedSubscription
	for _, rec := range saved {
		_, running := s.subs[rec.ID]
		if _, waiting := s.reserved[rec.ID]; !running && !waiting {
			stale = append(stale, rec)
		}
	}
	live := s.subs[sub.id] == sub
	if live {
		s.stored[sub.id] = true
	}
	s.mu.Unlock()
	for _, rec := range stale {
		s.deleteLocked(rec.FabricIndex, rec.ID)
	}
	if !live {
		return // ended, or replaced, before it was saved
	}
	rec := SavedSubscription{
		FabricIndex: sub.session.FabricIndex(), ID: sub.id, PeerNode: sub.session.PeerNodeID(),
		Paths: sub.paths, FabricFiltered: sub.filtered,
		Min: uint16(sub.min / time.Second), Max: uint16(sub.max / time.Second), Peer: sub.session.Peer(),
	}
	if err := s.Saved.SaveSubscription(rec); err != nil {
		s.Log.Error("Could not save subscription", "err", err, "subscription", sub.id)
	}
}

// Forget deletes a saved subscription's record: one that was replaced, whose client refused it, or
// that resuming gave up on. A session with no fabric never had one.
func (s *Server) Forget(fabricIndex uint8, id uint32) {
	if s.Saved == nil || fabricIndex == 0 {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.deleteLocked(fabricIndex, id)
}

// deleteLocked deletes a record, with saveMu held.
func (s *Server) deleteLocked(fabricIndex uint8, id uint32) {
	s.mu.Lock()
	delete(s.reserved, id)
	delete(s.stored, id)
	s.mu.Unlock()
	if err := s.Saved.DeleteSubscription(fabricIndex, id); err != nil {
		s.Log.Error("Could not delete saved subscription", "err", err, "subscription", id)
	}
}

// RemoveFabric deletes the saved subscriptions of a fabric the node has left.
func (s *Server) RemoveFabric(index uint8) {
	if s.Saved == nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	saved, err := s.Saved.Subscriptions()
	if err != nil {
		s.Log.Error("Could not read saved subscriptions", "err", err)
		return
	}
	for _, rec := range saved {
		if rec.FabricIndex == index {
			s.deleteLocked(rec.FabricIndex, rec.ID)
		}
	}
}

// Resume resumes a saved subscription on session, a new CASE session to its subscriber: it sends a
// priming report under the saved ID, then runs it as if the subscriber had just subscribed. It
// returns an error wrapping ErrRefused if the subscriber refuses it, and ErrForgotten if it was
// forgotten in the meantime.
func (s *Server) Resume(ctx context.Context, session *transport.Session, saved SavedSubscription) error {
	sub := &subscription{
		id: saved.ID, session: session, paths: saved.Paths, filtered: saved.FabricFiltered,
		min: time.Duration(saved.Min) * time.Second, max: time.Duration(max(saved.Max, 1)) * time.Second,
		dirty: map[Path]bool{}, wake: make(chan struct{}, 1),
	}
	if !s.Waiting(saved.ID) {
		return ErrForgotten
	}
	runCtx, stop, err := s.take(ctx, sub)
	if err != nil {
		return err
	}
	ex, err := session.NewExchange()
	if err == nil {
		// On the subscription's own context, so a client that replaces it meanwhile cuts it short.
		err = s.prime(runCtx, ex, sub, nil)
		ex.Close()
	}
	s.mu.Lock()
	_, waiting := s.reserved[saved.ID]
	delete(s.reserved, sub.id)
	s.mu.Unlock()
	switch {
	case err == nil && !waiting:
		err = ErrForgotten // while it primed
	case errors.Is(err, StatusInvalidSubscription):
		err = fmt.Errorf("%w: %w", ErrRefused, err)
	case err != nil && !waiting:
		err = ErrForgotten
	}
	if err != nil {
		stop()
		s.endSubscription(sub)
		if !errors.Is(err, ErrRefused) && !errors.Is(err, ErrForgotten) {
			s.mu.Lock()
			s.reserved[saved.ID] = saved // still to resume, on the next try
			s.mu.Unlock()
		}
		return err
	}
	go s.serveSubscription(runCtx, sub, stop)
	return nil
}
