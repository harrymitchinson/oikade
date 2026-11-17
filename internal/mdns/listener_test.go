package mdns

import (
	"bytes"
	"os"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// TestListenerHearsWhatItSends multicasts an mDNS query on the real network, for a name nobody
// has, and checks that the Listener hears it come back. Some networks don't loop multicast back,
// and a test shouldn't multicast wherever it runs, so it runs only with OIKADE_TEST_MULTICAST=1.
func TestListenerHearsWhatItSends(t *testing.T) {
	if os.Getenv("OIKADE_TEST_MULTICAST") != "1" {
		t.Skip("OIKADE_TEST_MULTICAST isn't 1")
	}
	l, err := ListenMulticast()
	if err != nil {
		t.Fatal(err)
	}
	want := pack(t, dnsmessage.Message{Questions: []dnsmessage.Question{{
		Name: mustName("_oikade-listener-test._udp.local."), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET,
	}}})
	heard := make(chan error, 1)
	go func() {
		b := make([]byte, MaxPacket)
		for {
			n, err := l.Read(b)
			if err != nil {
				heard <- err
				return
			}
			if bytes.Equal(b[:n], want) {
				heard <- nil
				return
			}
		}
	}()
	if err := l.Send(want); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-heard:
		if err != nil {
			t.Errorf("Read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("the query didn't come back")
	}
	if err := l.Close(); err != nil {
		t.Error(err)
	}
}
