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

// newSubscriptionID returns an ID that no subscription has, running or saved.
func (s *Server) newSubscriptionID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		id := randomSubscriptionID()
		_, running := s.subs[id]
		if _, saved := s.reserved[id]; !running && !saved {
			return id
		}
	}
}

// LoadSubscriptions returns the saved subscriptions, and keeps their IDs from new ones. Call it
// once before serving. Each is to be resumed with Resume, or given up with Forget.
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
	s.reserved = map[uint32]SavedSubscription{}
	for _, sub := range saved {
		s.reserved[sub.ID] = sub
	}
	return saved, nil
}

// save keeps sub's record, for a CASE session on a fabric. Records of subscriptions that have
// ended go first, so there are never more than the subscriptions the server can hold. A record
// that can't be written costs only resuming it after a restart.
func (s *Server) save(sub *subscription) {
	if s.Saved == nil || sub.session.AuthMode() != transport.AuthCASE || sub.session.FabricIndex() == 0 {
		return
	}
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
	s.mu.Unlock()
	for _, rec := range stale {
		s.forget(rec.FabricIndex, rec.ID)
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
// that resuming gave up on.
func (s *Server) Forget(fabricIndex uint8, id uint32) { s.forget(fabricIndex, id) }

func (s *Server) forget(fabricIndex uint8, id uint32) {
	if s.Saved == nil {
		return
	}
	s.mu.Lock()
	delete(s.reserved, id)
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
	saved, err := s.Saved.Subscriptions()
	if err != nil {
		s.Log.Error("Could not read saved subscriptions", "err", err)
		return
	}
	for _, rec := range saved {
		if rec.FabricIndex == index {
			s.forget(rec.FabricIndex, rec.ID)
		}
	}
}

// errForgotten means a saved subscription was forgotten while it waited to be resumed, as when its
// subscriber subscribed again first.
var errForgotten = errors.New("saved subscription was forgotten")

// ErrRefused means a subscriber answered a resumed subscription's priming report with
// INVALID_SUBSCRIPTION: it has forgotten it, and will subscribe again itself.
var ErrRefused = errors.New("subscriber refused the resumed subscription")

// Resume resumes a saved subscription on session, a new CASE session to its subscriber: it sends a
// priming report under the saved ID, then runs it as if the subscriber had just subscribed.
func (s *Server) Resume(ctx context.Context, session *transport.Session, saved SavedSubscription) error {
	sub := &subscription{
		id: saved.ID, session: session, paths: saved.Paths, filtered: saved.FabricFiltered,
		min: time.Duration(saved.Min) * time.Second, max: time.Duration(max(saved.Max, 1)) * time.Second,
		dirty: map[Path]bool{}, wake: make(chan struct{}, 1),
	}
	s.mu.Lock()
	_, waiting := s.reserved[saved.ID]
	s.mu.Unlock()
	if !waiting {
		return errForgotten
	}
	runCtx, stop, err := s.take(ctx, sub)
	if err != nil {
		return err
	}
	ex, err := session.NewExchange()
	if err == nil {
		err = s.prime(ctx, ex, sub, nil)
		ex.Close()
	}
	if err != nil {
		stop()
		s.endSubscription(sub)
		if errors.Is(err, StatusInvalidSubscription) {
			return fmt.Errorf("%w: %w", ErrRefused, err)
		}
		return err
	}
	s.mu.Lock()
	_, waiting = s.reserved[saved.ID]
	delete(s.reserved, sub.id)
	s.mu.Unlock()
	if !waiting { // forgotten while it primed
		stop()
		s.endSubscription(sub)
		return errForgotten
	}
	go s.serveSubscription(runCtx, sub, stop)
	return nil
}
