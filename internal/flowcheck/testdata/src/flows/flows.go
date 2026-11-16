package flows

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade"
)

func home(h *oikade.Home) {
	h.Flow("a", func(r *oikade.FlowRun) {
		_ = time.Now() // want `time.Now inside a flow varies between runs`
		go func() {}() // want `a goroutine inside a flow breaks its replay`
	})
}

func named(r *oikade.FlowRun) {
	time.Sleep(time.Second)     // want `time.Sleep inside a flow`
	_ = time.Since(r.Now())     // want `time.Since inside a flow`
	<-time.After(time.Second)   // want `time.After inside a flow`
	func() { _ = time.Now() }() // want `time.Now inside a flow`
	now := time.Now             // want `time.Now inside a flow`
	_ = now
	for range map[string]int{} { // want `ranging over a map inside a flow`
	}
	for range []int{1} {
	}
	helper(r)
}

func helper(r *oikade.FlowRun) {
	go tick() // want `a goroutine inside a flow`
}

func mapIterators(r *oikade.FlowRun) {
	m := map[string]int{}
	for range maps.Keys(m) { // want `ranging over a map inside a flow`
	}
	for range maps.Values(m) { // want `ranging over a map inside a flow`
	}
	for range maps.All(m) { // want `ranging over a map inside a flow`
	}
	for range maps.Keys(m) { // want `ranging over a map inside a flow`
	}
	for range slices.Collect(maps.Keys(m)) { // want `ranging over a map inside a flow`
	}
	for range slices.Sorted(maps.Keys(m)) {
	}
}

type (
	aliasedRun    = oikade.FlowRun
	aliasedRunPtr = *oikade.FlowRun
)

func aliased(r *aliasedRun) {
	_ = time.Now() // want `time.Now inside a flow`
	_, _ = r.Do("stamp", func(ctx context.Context) (time.Time, error) {
		return time.Now(), nil
	})
}

func aliasedPointer(r aliasedRunPtr) {
	_ = time.Now() // want `time.Now inside a flow`
	_, _ = r.Do("stamp", func(ctx context.Context) (time.Time, error) {
		return time.Now(), nil
	})
}

// tick is outside any flow.
func tick() time.Time { return time.Now() }

func outside(ctx context.Context) time.Time {
	go tick()
	time.Sleep(time.Second)
	return time.Now()
}

func allowed(r *oikade.FlowRun) {
	_ = tick()
	_ = time.Hour.String()
	_ = r.Now().Add(time.Minute)
	_, _ = r.Do("stamp", func(ctx context.Context) (time.Time, error) {
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
		return time.Now(), nil
	})
	// Do with its type given.
	_, _ = r.Do[time.Time]("stamp", func(ctx context.Context) (time.Time, error) {
		return time.Now(), nil
	})
	// Its name still counts as the flow's.
	_, _ = r.Do(time.Now().String(), func(ctx context.Context) (int, error) { // want `time.Now inside a flow`
		return 0, nil
	})
}
