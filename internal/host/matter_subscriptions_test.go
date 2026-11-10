package host

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/store"
)

// TestSubscriptionStore: saved subscriptions come back as they went in after the store is opened
// again, a deleted one doesn't, and a record that doesn't read is dropped.
func TestSubscriptionStore(t *testing.T) {
	dir := t.TempDir()
	data, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	subs := subscriptionStore{data.Namespace(matterSubscriptionsNamespace)}
	hub := im.SavedSubscription{
		FabricIndex: 1, ID: 0xDEADBEEF, PeerNode: 0x20317E3F, FabricFiltered: true, Min: 1, Max: 60,
		Paths: []im.AttributePath{{Endpoint: 3, Cluster: 6, Attribute: 0}, {WildEndpoint: true, Cluster: 0x39, WildAttribute: true}},
		Peer:  netip.MustParseAddrPort("[fe80::15%en0]:5540"),
	}
	phone := im.SavedSubscription{FabricIndex: 2, ID: 7, PeerNode: 9, Max: 60, Peer: netip.MustParseAddrPort("192.0.2.4:5540")}
	for _, s := range []im.SavedSubscription{hub, phone} {
		if err := subs.SaveSubscription(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := subs.DeleteSubscription(phone.FabricIndex, phone.ID); err != nil {
		t.Fatal(err)
	}
	if err := data.Namespace(matterSubscriptionsNamespace).Put("01/00000001", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	data, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	subs = subscriptionStore{data.Namespace(matterSubscriptionsNamespace)}
	got, err := subs.Subscriptions()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []im.SavedSubscription{hub}) {
		t.Errorf("got %+v, want only %+v", got, hub)
	}
	if keys, _ := data.Namespace(matterSubscriptionsNamespace).Keys(); len(keys) != 1 {
		t.Errorf("keys %v: the unreadable record is still there", keys)
	}
}
