package flows

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/framework"
)

var _ = framework.Flow{
	Run: func(r *framework.FlowRun) error {
		_ = time.Now() // want `time.Now inside a flow varies between runs`
		go func() {}() // want `a goroutine inside a flow breaks its replay`
		return nil
	},
}

func named(r *framework.FlowRun) error {
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
	return helper(r)
}

func helper(r *framework.FlowRun) error {
	go tick() // want `a goroutine inside a flow`
	return nil
}

func mapIterators(r *framework.FlowRun) error {
	m := map[string]int{}
	for range maps.Keys(m) { // want `ranging over a map inside a flow`
	}
	for range maps.Values(m) { // want `ranging over a map inside a flow`
	}
	for range maps.All(m) { // want `ranging over a map inside a flow`
	}
	for range (maps.Keys(m)) { // want `ranging over a map inside a flow`
	}
	for range slices.Sorted(maps.Keys(m)) {
	}
	return nil
}

type (
	aliasedRun    = framework.FlowRun
	aliasedRunPtr = *framework.FlowRun
)

func aliased(r *aliasedRun) error {
	_ = time.Now() // want `time.Now inside a flow`
	return r.Do("stamp", nil, func(ctx context.Context) error {
		_ = time.Now()
		return nil
	})
}

func aliasedPointer(r aliasedRunPtr) error {
	_ = time.Now() // want `time.Now inside a flow`
	return r.Do("stamp", nil, func(ctx context.Context) error {
		_ = time.Now()
		return nil
	})
}

// tick is outside any flow.
func tick() time.Time { return time.Now() }

func outside(ctx context.Context) time.Time {
	go tick()
	time.Sleep(time.Second)
	return time.Now()
}

func allowed(r *framework.FlowRun) error {
	var stamp time.Time
	_ = tick()
	_ = time.Hour.String()
	_ = r.Now().Add(time.Minute)
	return r.Do("stamp", &stamp, func(ctx context.Context) error {
		stamp = time.Now()
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
		return nil
	})
}
