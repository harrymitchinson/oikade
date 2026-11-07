package securechannel

import (
	"bytes"
	"errors"
	"testing"
)

// A device whose Sigma2_Resume doesn't prove it holds the resumed secret fails the CASE, and its
// resumption is forgotten, so the next CASE with it is a full one rather than the same failure.
func TestAResumeTheDeviceCantProveIsForgotten(t *testing.T) {
	store := NewResumptionStore(0)
	res := resumption{id: [16]byte{7}, fabric: 1, node: deviceNode, secret: bytes.Repeat([]byte{3}, 32)}
	store.save(res)
	other := resumption{id: [16]byte{8}, fabric: 1, node: controllerNode, secret: res.secret}
	store.save(other)
	i := &caseInitiator{f: &fabricKeys{Fabric: &Fabric{Index: 1}}, peerNode: deviceNode, c: CASEConfig{Resumption: store}}

	payload := (&sigma2Resume{resumptionID: bytes.Repeat([]byte{9}, 16), resumeMIC: make([]byte, 16), responderSessionID: 1}).encode()
	if _, err := i.resumed(timeout(t), random(), res, payload); !errors.Is(err, ErrCASEFailed) {
		t.Fatalf("resumed: %v, want ErrCASEFailed", err)
	}
	if _, ok := store.byPeer(1, deviceNode); ok {
		t.Error("the failed resumption is still remembered")
	}
	if _, ok := store.byPeer(1, controllerNode); !ok {
		t.Error("another peer's resumption was forgotten too")
	}
}
