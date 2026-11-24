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
		// Slack for when each write reaches the fake, after the plugin's clock paced it.
		if gap := puts[i].at.Sub(puts[i-1].at); gap < slow.pace/2 {
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
	// Set says so, and the SDK logs it.
	if n := b.logs.count("Could not write to Hue light"); n != 0 {
		t.Errorf("logged %d refused writes, want none: %s", n, b.logs)
	}
}

func TestAWriteRefusedAfterItsSetReturnedIsLogged(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	slow := fast
	slow.pace, slow.set = 200*time.Millisecond, 10*time.Millisecond
	b := start(t, f, options{timing: slow})
	b.waitOnline()
	f.refuseWrites("device (light) has communication issues, command (on) may not have effect")
	_ = b.set(hallLight, contract.On, true)
	if err := b.set(deskLamp, contract.On, false); err != nil {
		t.Fatalf("got %v, want nil while it waits its turn", err)
	}
	b.eventually("the refusal logged", func() bool { return b.logs.count("Could not write to Hue light") == 1 })
}

func TestAWriteTheBridgeDoesntAnswerEndsTheSession(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	slow := fast
	slow.write = 100 * time.Millisecond
	b := start(t, f, options{timing: slow})
	b.waitOnline()
	b.waitAvailability(hallLight, "online")
	f.hangWrites()
	if err := b.set(hallLight, contract.On, true); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("got %v, want it timed out", err)
	}
	b.waitAvailability(hallLight, "offline")
	b.waitProblem("could not reach the Hue bridge " + bridgeOne + ": the bridge didn't answer a write")
}

func TestAReplacedWriteKeepsALightsOrder(t *testing.T) {
	f := newFakeBridge(t, bridgeOne)
	slow := fast
	slow.pace, slow.set = 300*time.Millisecond, 10*time.Millisecond
	b := start(t, f, options{timing: slow})
	b.waitOnline()
	// The first goes at once, and the rest wait their turn.
	for _, w := range []struct {
		capability string
		value      any
	}{{contract.Brightness, 10.0}, {contract.On, true}, {contract.Brightness, 30.0}, {contract.On, false}} {
		if err := b.set(deskLamp, w.capability, w.value); err != nil {
			t.Fatal(err)
		}
	}
	b.waitState(deskLamp, `{"brightness":30,"on":false}`)
	want := []string{deskLampID + ` {"dimming":{"brightness":10}}`, deskLampID + ` {"dimming":{"brightness":30}}`, deskLampID + ` {"on":{"on":false}}`}
	b.eventually("the writes", func() bool { return strings.Join(bodies(f.written()), "\n") == strings.Join(want, "\n") })
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
		{deskLamp, contract.Brightness, 150.0, "can't set brightness to 150 on " + deskLamp + ": it's from 0 to 100"},
		{deskLamp, contract.Brightness, -1.0, "it's from 0 to 100"},
		{hallSensor, contract.Occupied, true, hallSensor + " has no light to set"},
		{"0x00178801ffffffff", contract.On, true, "no Hue device 0x00178801ffffffff"},
		{hallLight, contract.On, 1.0, "can't set on to 1 on " + hallLight},
		{hallLight, contract.Brightness, true, "can't set brightness to true on " + hallLight},
		{hallLight, contract.Temperature, 21.0, "can't set temperature to 21 on " + hallLight},
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
	// Set said so, so only the SDK logs it.
	if n := b.logs.count("Dropped Hue write"); n != 0 {
		t.Errorf("logged %d dropped writes, want none: %s", n, b.logs)
	}
}
