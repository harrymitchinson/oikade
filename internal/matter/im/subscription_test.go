package im

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// TestSubscriptionReportsAtMostEveryMinInterval: a change waits until the min interval since the
// last report has passed, on a fake clock, and a change right after a report waits a whole min
// interval again (Core spec, 8.5).
func TestSubscriptionReportsAtMostEveryMinInterval(t *testing.T) {
	node, c := onOffNode()
	clk := clock.NewFake(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	client, _, _, _ := pairWith(t, &Server{Node: node, Clock: clk, Log: logging.Test(t)}, transport.SecureSessionConfig{Auth: transport.AuthPASE})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const minInterval = 5 * time.Second
	_, _, _, reports, err := client.Subscribe(ctx, uint16(minInterval/time.Second), 60, AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0})
	if err != nil {
		t.Fatal(err)
	}
	waitsFor := func(what string, want time.Time) {
		t.Helper()
		testkit.Eventually(t, what, func() bool {
			got, ok := clk.NextWait()
			return ok && got.Equal(want)
		})
	}
	noReport := func(when string) {
		t.Helper()
		select {
		case r := <-reports:
			t.Fatalf("reported %v %s", r, when)
		case <-time.After(100 * time.Millisecond):
		}
	}
	report := func(when string) {
		t.Helper()
		select {
		case r := <-reports:
			if len(r) != 1 || r[0].Path.Attribute != 0 {
				t.Fatalf("reported %v %s, want OnOff", r, when)
			}
		case <-ctx.Done():
			t.Fatalf("never reported %s", when)
		}
	}

	last := clk.Now() // the priming report
	waitsFor("the keep-alive's wait", last.Add(60*time.Second))
	node.Update(func() { c.Changed(0) })
	waitsFor("the change's wait for the min interval", last.Add(minInterval))
	clk.SetTime(last.Add(minInterval - time.Nanosecond))
	noReport("before the min interval since the priming report")
	clk.Step(time.Nanosecond)
	report("at the min interval since the priming report")

	last = clk.Now()
	waitsFor("the keep-alive's wait after the report", last.Add(60*time.Second))
	node.Update(func() { c.Changed(0) })
	waitsFor("the next change's wait for a whole min interval", last.Add(minInterval))
	clk.SetTime(last.Add(minInterval - time.Nanosecond))
	noReport("before the min interval since the last report")
	clk.Step(time.Nanosecond)
	report("at the min interval since the last report")
}

// TestSubscriptionsAreCapped: the server holds maxSubscriptions, and refuses the next with
// RESOURCE_EXHAUSTED, keeping the ones it has.
func TestSubscriptionsAreCapped(t *testing.T) {
	node, _ := onOffNode()
	client, _, srv, _ := pairServer(t, node)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	path := AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0}
	for i := range maxSubscriptions {
		if _, _, _, _, err := client.subscribe(ctx, true, 0, 60, path); err != nil {
			t.Fatalf("subscription %d of %d: %v", i+1, maxSubscriptions, err)
		}
	}
	_, _, _, _, err := client.subscribe(ctx, true, 0, 60, path)
	if !errors.Is(err, StatusResourceExhausted) {
		t.Fatalf("subscription %d: %v, want %v", maxSubscriptions+1, err, StatusResourceExhausted)
	}
	if n := srv.Subscriptions(); n != maxSubscriptions {
		t.Fatalf("%d subscriptions after one too many, want %d", n, maxSubscriptions)
	}
}
