package host

import (
	"encoding/json"
	"fmt"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/store"
)

// matterSubscriptionsNamespace is where the store keeps controllers' subscriptions to the bridge,
// so that it resumes them after a restart (OIK-209).
const matterSubscriptionsNamespace = "matter-subscriptions"

// subscriptionStore keeps the bridge's subscriptions in the store, one JSON value each, keyed by
// fabric index and subscription ID. Each write is on disk when it returns.
type subscriptionStore struct{ values *store.Namespace }

func subscriptionKey(fabricIndex uint8, id uint32) string {
	return fmt.Sprintf("%02X/%08X", fabricIndex, id)
}

// SaveSubscription implements im.SubscriptionStore.
func (s subscriptionStore) SaveSubscription(sub im.SavedSubscription) error {
	b, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	return s.values.Put(subscriptionKey(sub.FabricIndex, sub.ID), b)
}

// DeleteSubscription implements im.SubscriptionStore.
func (s subscriptionStore) DeleteSubscription(fabricIndex uint8, id uint32) error {
	return s.values.Delete(subscriptionKey(fabricIndex, id))
}

// Subscriptions implements im.SubscriptionStore. A record it can't read is left out, and deleted:
// it would never resume.
func (s subscriptionStore) Subscriptions() ([]im.SavedSubscription, error) {
	keys, err := s.values.Keys()
	if err != nil {
		return nil, err
	}
	out := make([]im.SavedSubscription, 0, len(keys))
	for _, k := range keys {
		b, ok, err := s.values.Get(k)
		if err != nil {
			return nil, err
		}
		var sub im.SavedSubscription
		if !ok || json.Unmarshal(b, &sub) != nil || subscriptionKey(sub.FabricIndex, sub.ID) != k {
			if err := s.values.Delete(k); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, sub)
	}
	return out, nil
}
