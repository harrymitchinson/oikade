package im

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"oikade.dev/internal/clock"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/matter/tlv"
	"oikade.dev/internal/matter/transport"
)

// TestTimedRequestExpires: the Invoke after a Timed Request runs if it arrives within the request's
// timeout, and is refused with TIMEOUT, unrun, if it arrives a moment later (Core spec, 8.7.2).
func TestTimedRequestExpires(t *testing.T) {
	var invoked atomic.Int32
	c := &Cluster{ID: 6, Revision: 1, Commands: []Command{{ID: 1, Timed: true, Invoke: func(*Request, *tlv.Reader) (*Response, error) {
		invoked.Add(1)
		return nil, nil
	}}}}
	node := &Node{}
	if err := node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}}); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	client, _, _, _ := pairWith(t, &Server{Node: node, Clock: clk, Log: logging.Test(t)}, transport.SecureSessionConfig{Auth: transport.AuthPASE})
	const timeoutMs = 500

	for _, tc := range []struct {
		name    string
		delay   time.Duration // between the Timed Request's answer and the Invoke
		want    error
		invoked int32
	}{
		{"at the timeout", timeoutMs * time.Millisecond, nil, 1},
		{"after the timeout", timeoutMs*time.Millisecond + time.Nanosecond, StatusTimeout, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoked.Store(0)
			// Invoke writes the command's fields once the Timed Request has been answered, and the
			// server's deadline set, so the clock moves between the two.
			res, err := client.Invoke(t.Context(), CommandPath{Endpoint: 1, Cluster: 6, Command: 1}, func(*tlv.Writer) {
				clk.Step(tc.delay)
			}, true, timeoutMs)
			if !errors.Is(err, tc.want) || (err == nil && res.Status != StatusSuccess) {
				t.Fatalf("invoke %v after a %d ms Timed Request: %+v, %v, want %v", tc.delay, timeoutMs, res, err, tc.want)
			}
			if n := invoked.Load(); n != tc.invoked {
				t.Fatalf("the command ran %d times, want %d", n, tc.invoked)
			}
		})
	}
}
