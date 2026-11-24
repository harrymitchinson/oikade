package hue

import (
	"strings"
	"testing"
	"time"

	"oikade.dev/contract/v1"
)

const (
	hallLightID = "d3afe652-b85c-4d64-ad5d-f8fd5469b415"
	deskLampID  = "027ae662-aaac-4595-acc6-03bb15e293cf"
	heaterID    = "c3d2e1f0-1111-4a4a-8b8b-0c0c0c0c0c03"
)

func (b *bench) set(id, capability string, value any) error {
	b.t.Helper()
	return b.handler.Set(b.t.Context(), nil, contract.SetCommand{ID: id, Capability: capability, Value: value})
}

func bodies(puts []put) []string {
	out := make([]string, 0, len(puts))
	for _, p := range puts {
		out = append(out, p.light+" "+p.body)
	}
	return out
}

func TestAWriteSendsTheChangeAndItsEventLands(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	if err := b.set(hallLight, contract.On, true); err != nil {
		t.Fatal(err)
	}
	b.waitState(hallLight, `{"on":true}`)
	if err := b.set(deskLamp, contract.Brightness, 42.0); err != nil {
		t.Fatal(err)
	}
	b.waitState(deskLamp, `{"brightness":42,"on":true}`)
	want := []string{hallLightID + ` {"on":{"on":true}}`, deskLampID + ` {"dimming":{"brightness":42}}`}
	if got := bodies(f.written()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

func TestWritesArePaced(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	slow := fast
	slow.pace = 100 * time.Millisecond
	b := start(t, f, options{timing: slow})
	b.waitOnline()
	errs := make(chan error, 3)
	for _, id := range []string{hallLight, deskLamp, heaterPlug} {
		go func() { errs <- b.set(id, contract.On, false) }()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	puts := f.written()
	if len(puts) != 3 {
		t.Fatalf("wrote %q, want 3 writes", bodies(puts))
	}
	for i := 1; i < len(puts); i++ {
		// A little slack for the clock the fake reads when the write arrives.
		if gap := puts[i].at.Sub(puts[i-1].at); gap < slow.pace-10*time.Millisecond {
			t.Errorf("write %d came %s after the one before, want %s at least", i, gap, slow.pace)
		}
	}
}

func TestANewerWriteReplacesOneStillWaiting(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	slow := fast
	slow.pace, slow.set = 300*time.Millisecond, 20*time.Millisecond
	b := start(t, f, options{timing: slow})
	b.waitOnline()
	// The first goes at once. The rest wait for the pace, and Set returns before they go.
	for _, v := range []float64{10, 20, 30} {
		if err := b.set(deskLamp, contract.Brightness, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.set(hallLight, contract.On, true); err != nil {
		t.Fatal(err)
	}
	b.waitState(hallLight, `{"on":true}`)
	b.waitState(deskLamp, `{"brightness":30,"on":true}`)
	want := []string{deskLampID + ` {"dimming":{"brightness":10}}`, deskLampID + ` {"dimming":{"brightness":30}}`, hallLightID + ` {"on":{"on":true}}`}
	if got := bodies(f.written()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

func TestAWriteTheBridgeRefusesSaysWhy(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	f.refuseWrites("device (light) has communication issues, command (on) may not have effect")
	err := b.set(hallLight, contract.On, true)
	if err == nil || !strings.Contains(err.Error(), "the bridge answered 404 Not Found: device (light) has communication issues") {
		t.Errorf("got %v, want the bridge's refusal", err)
	}
	if n := b.logs.count("Could not write to Hue light"); n != 1 {
		t.Errorf("logged %d refused writes, want 1: %s", n, b.logs)
	}
}

func TestAWriteFailsAtOnceWithoutTheBridge(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	f.setRefuse(503)
	f.endStream()
	b.waitAvailability(hallLight, "offline")
	began := time.Now()
	if err := b.set(hallLight, contract.On, true); err == nil || !strings.Contains(err.Error(), "can't reach the Hue bridge "+bridgeOne) {
		t.Errorf("got %v, want can't reach", err)
	}
	if d := time.Since(began); d > time.Second {
		t.Errorf("took %s to fail", d)
	}
	if n := len(f.written()); n != 0 {
		t.Errorf("wrote %d times", n)
	}
}

func TestAWriteThePluginCantSendIsRefused(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	b := start(t, f, options{})
	b.waitOnline()
	for _, c := range []struct {
		id, capability string
		value          any
		want           string
	}{
		{deskLamp, contract.Brightness, 150.0, "brightness is from 0 to 100"},
		{deskLamp, contract.Brightness, -1.0, "brightness is from 0 to 100"},
		{hallSensor, contract.Occupied, true, hallSensor + " has no light to set"},
		{"0x00178801ffffffff", contract.On, true, "no Hue device 0x00178801ffffffff"},
		{hallLight, contract.On, 1.0, "can't set on to 1"},
		{hallLight, contract.Brightness, true, "brightness takes a number"},
	} {
		if err := b.set(c.id, c.capability, c.value); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("set %s %s %v: got %v, want %q", c.id, c.capability, c.value, err, c.want)
		}
	}
	if n := len(f.written()); n != 0 {
		t.Errorf("wrote %d times", n)
	}
}

func TestAWriteWaitingWhenTheSessionEndsIsDropped(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	slow := fast
	slow.pace, slow.set = time.Hour, 5*time.Second
	b := start(t, f, options{timing: slow})
	b.waitOnline()
	if err := b.set(hallLight, contract.On, true); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() {
		waiting <- b.handler.Set(t.Context(), nil, contract.SetCommand{ID: deskLamp, Capability: contract.On, Value: false})
	}()
	b.eventually("the write waiting", func() bool {
		b.handler.mu.Lock()
		w := b.handler.writer
		b.handler.mu.Unlock()
		if w == nil {
			return false
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.waiting) == 1
	})
	f.setRefuse(503)
	f.endStream()
	if err := <-waiting; err == nil || !strings.Contains(err.Error(), "lost the Hue bridge before the write went") {
		t.Errorf("got %v, want it lost", err)
	}
	b.eventually("the drop logged", func() bool { return b.logs.count("Dropped Hue write") == 1 })
}
