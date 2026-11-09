package mdns

import (
	"bytes"
	"testing"
	"time"
)

func TestListenerHearsWhatItSends(t *testing.T) {
	l, err := ListenMulticast()
	if err != nil {
		t.Skipf("No multicast here: %v", err)
	}
	heard := make(chan bool, 1)
	want := []byte("oikade listener test")
	go func() {
		b := make([]byte, maxPacket)
		for {
			n, err := l.Read(b)
			if err != nil {
				heard <- false
				return
			}
			if bytes.Equal(b[:n], want) {
				heard <- true
				return
			}
		}
	}()
	if err := l.Send(want); err != nil {
		_ = l.Close()
		t.Skipf("Can't send multicast here: %v", err)
	}
	select {
	case ok := <-heard:
		if !ok {
			t.Error("The read failed")
		}
	case <-time.After(2 * time.Second):
		t.Log("No multicast came back, which some networks don't do")
	}
	if err := l.Close(); err != nil {
		t.Error(err)
	}
}
