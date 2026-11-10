package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/mdns"
	"github.com/harrymitchinson/oikade/internal/matter/securechannel"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Resuming's timings: the first retry, and how long finding a subscriber may take each time.
const (
	firstResumeRetry = time.Second
	resolveTimeout   = 3 * time.Second
)

// resumer brings back, after a restart, the subscriptions controllers held: it opens CASE to each
// subscriber and reports under the old subscription ID, so a controller that missed the restart
// hears from the node within seconds, rather than when its own subscription times out (OIK-209).
type resumer struct {
	clock  clock.Clock
	server *im.Server
	// dial opens a CASE session to a saved subscription's subscriber.
	dial func(ctx context.Context, saved im.SavedSubscription) (*transport.Session, error)
	// end ends a session that a resume failed on.
	end func(*transport.Session)
	log *slog.Logger
}

// resume tries to resume saved until it works, the subscriber refuses it, or one max interval has
// passed, by when the subscriber has noticed the node is gone and will subscribe again itself. It
// waits 1, 2, 4… seconds between tries. Then it deletes the record, unless ctx ended first, as
// when the node stops, which keeps it for the next start.
func (r *resumer) resume(ctx context.Context, saved im.SavedSubscription) {
	start := r.clock.Now()
	limit := time.Duration(max(saved.Max, 1)) * time.Second
	wait := firstResumeRetry
	node := fmt.Sprintf("%016X", saved.PeerNode)
	for tries := 1; ; tries++ {
		if !r.server.Waiting(saved.ID) {
			return // forgotten, as when its subscriber subscribed again first
		}
		err := r.try(ctx, saved)
		if err == nil {
			r.log.Info("Resumed subscription", "subscription", saved.ID, "node", node, "fabric", saved.FabricIndex,
				"took", r.clock.Now().Sub(start).String(), "tries", tries)
			return
		}
		if ctx.Err() != nil || errors.Is(err, im.ErrForgotten) {
			return
		}
		r.log.Debug("Could not resume subscription", "err", err, "subscription", saved.ID, "node", node, "tries", tries)
		if errors.Is(err, im.ErrRefused) || r.clock.Now().Add(wait).Sub(start) > limit {
			r.server.Forget(saved.FabricIndex, saved.ID)
			r.log.Info("Gave up resuming subscription", "subscription", saved.ID, "node", node, "fabric", saved.FabricIndex,
				"tries", tries, "reason", err.Error())
			return
		}
		t := r.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		wait *= 2
	}
}

// try is one attempt: a CASE session, then the priming report.
func (r *resumer) try(ctx context.Context, saved im.SavedSubscription) error {
	s, err := r.dial(ctx, saved)
	if err != nil {
		return err
	}
	if err := r.server.Resume(ctx, s, saved); err != nil {
		r.end(s)
		return err
	}
	return nil
}

// dialSubscriber finds a subscriber by operational discovery, or else at its last address, and
// opens CASE to it on the node's fabric.
func (n *Node) dialSubscriber(ctx context.Context, tr *transport.Transport, responder advertiser, saved im.SavedSubscription) (*transport.Session, error) {
	f, ok := n.table.Fabric(saved.FabricIndex)
	if !ok {
		return nil, fmt.Errorf("%w: fabric %d is gone", im.ErrRefused, saved.FabricIndex)
	}
	var sf *securechannel.Fabric
	for _, c := range n.table.CASE().Fabrics() {
		if c.Index == saved.FabricIndex {
			sf = c
		}
	}
	if sf == nil {
		return nil, fmt.Errorf("%w: fabric %d is gone", im.ErrRefused, saved.FabricIndex)
	}
	addrs := []netip.AddrPort{}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	found, err := responder.ResolveOperational(rctx, f.CompressedFabricID, saved.PeerNode)
	cancel()
	if err == nil {
		addrs = append(addrs, found.Addrs...)
	} else if !errors.Is(err, mdns.ErrNotFound) {
		return nil, err
	}
	if saved.Peer.IsValid() && !slices.Contains(addrs, saved.Peer) {
		addrs = append(addrs, saved.Peer)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("found no address for node %016X", saved.PeerNode)
	}
	var errs []error
	for _, a := range addrs {
		s, err := securechannel.EstablishCASE(ctx, tr, a, sf, saved.PeerNode, securechannel.CASEConfig{})
		if err == nil {
			n.sessions.add(s)
			return s, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", a, err))
	}
	return nil, errors.Join(errs...)
}
