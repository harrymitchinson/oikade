package engine

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// AverageStatesNamespace keeps an AverageState per Average, at its Key.
const AverageStatesNamespace = "averages"

// averageBuckets is how many buckets a window is kept in, so the average is right to within a
// bucket's share of the window.
const averageBuckets = 48

// Average is a rolling average of a device's number capability over a window. The engine keeps
// one for each in Engine.Averages, and a condition reads it as the capability Var names.
type Average struct {
	Device, Capability string
	Over               time.Duration
}

// Key is the average's name in AverageStates: "<device> <capability> <over>".
func (a Average) Key() ident.ID {
	return ident.ID{Name: a.Device + " " + a.Capability + " " + ShortDuration(a.Over)}
}

// Var is the name a condition reads the average by, among its device's capabilities: "temperature
// over 24h".
func (a Average) Var() string { return AverageVar(a.Capability, a.Over) }

// AverageVar is the name a condition reads an average of the capability name over d by.
func AverageVar(name string, d time.Duration) string {
	return name + " over " + ShortDuration(d)
}

// ShortDuration is d as "24h", "90m" or "1h30m", without the zero units time.Duration's String has.
func ShortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// bucket is how long one of the average's buckets is: at least a minute.
func (a Average) bucket() time.Duration {
	return max(a.Over/averageBuckets, time.Minute).Truncate(time.Minute)
}

// AverageState is what an average needs across a restart: the reading's time-weighted sum in
// buckets, and the last reading, which holds until the next.
type AverageState struct {
	Buckets []AverageBucket `json:"buckets,omitempty"`
	// Last is the last reading, at LastAt, and nil before the first.
	Last   *float64  `json:"last,omitempty"`
	LastAt time.Time `json:"lastAt,omitzero"`
}

// AverageBucket is the reading's sum over a bucket, in value-seconds, and how many seconds of the
// bucket it covers.
type AverageBucket struct {
	Start   time.Time `json:"start"`
	Sum     float64   `json:"sum"`
	Seconds float64   `json:"seconds"`
}

// AverageStates keeps AverageStates, by Average's Key.
type AverageStates = States[AverageState]

// averages are the engine's rolling averages, which take every reading of the capabilities they
// average, and are saved as each one changes.
type averages struct {
	mu sync.Mutex
	// ctx is Run's, which saves outlive.
	ctx    context.Context
	states map[Average]*averageState
	// saving are the states waiting to be saved, which one goroutine per average writes, latest
	// first, so an older one never lands after a newer.
	saving map[Average]*AverageState
}

type averageState struct {
	AverageState
	loaded bool
}

// add takes the reading from LastAt to at into the buckets.
func (s *AverageState) add(a Average, at time.Time) {
	if s.Last == nil || !at.After(s.LastAt) {
		return
	}
	size := a.bucket()
	for from := s.LastAt; from.Before(at); {
		start := from.Truncate(size)
		to := start.Add(size)
		if at.Before(to) {
			to = at
		}
		n := len(s.Buckets)
		if n == 0 || !s.Buckets[n-1].Start.Equal(start) {
			s.Buckets = append(s.Buckets, AverageBucket{Start: start})
			n++
		}
		seconds := to.Sub(from).Seconds()
		s.Buckets[n-1].Sum += *s.Last * seconds
		s.Buckets[n-1].Seconds += seconds
		from = to
	}
	s.LastAt = at
	// Buckets wholly before the window are of no more use.
	cut := at.Add(-a.Over).Truncate(size)
	i := 0
	for i < len(s.Buckets) && s.Buckets[i].Start.Before(cut) {
		i++
	}
	s.Buckets = s.Buckets[i:]
}

// at is the average at now, and false before the first reading. A window not yet full of readings
// averages what it has.
func (s AverageState) at(a Average, now time.Time) (float64, bool) {
	if s.Last == nil {
		return 0, false
	}
	s.Buckets = append([]AverageBucket(nil), s.Buckets...)
	s.add(a, now)
	var sum, seconds float64
	from := now.Add(-a.Over)
	for _, b := range s.Buckets {
		if b.Start.Add(a.bucket()).After(from) {
			sum, seconds = sum+b.Sum, seconds+b.Seconds
		}
	}
	if seconds == 0 {
		return *s.Last, true
	}
	return sum / seconds, true
}

// averageState returns the average's state, loading it the first time: from what was saved, or else
// from the reading now, if there is one, which is saved so a restart starts from it. Call with
// e.averages.mu held.
func (e *Engine) averageState(a Average, now time.Time) *averageState {
	av := &e.averages
	if av.states == nil {
		av.states = map[Average]*averageState{}
	}
	s := av.states[a]
	if s == nil {
		s = &averageState{}
		av.states[a] = s
	}
	if !s.loaded {
		s.loaded = true
		if saved, ok := e.AverageStates.Get(a.Key()); ok {
			s.AverageState = saved
		}
		if s.Last == nil {
			if snap, ok := e.Store.Get(ident.ID{Name: a.Device}); ok {
				if v, ok := snap.Values[a.Capability]; ok {
					f := capability.Float(v)
					s.Last, s.LastAt = &f, now
					e.saveAverage(a, s.AverageState)
				}
			}
		}
	}
	return s
}

// observeAverages takes a report's readings into the averages of their capabilities, and saves them.
func (e *Engine) observeAverages(changes []driver.Update, now time.Time) {
	if len(e.Averages) == 0 {
		return
	}
	e.averages.mu.Lock()
	defer e.averages.mu.Unlock()
	for _, u := range changes {
		for _, a := range e.Averages {
			if a.Device != u.Device.Name || a.Capability != u.Capability {
				continue
			}
			s := e.averageState(a, now)
			s.add(a, now)
			f := capability.Float(u.Value)
			s.Last, s.LastAt = &f, now
			e.saveAverage(a, s.AverageState)
		}
	}
}

// saveAverage saves an average's state apart from the engine, which can't wait for the host. Only
// the latest waiting state is saved. Call with e.averages.mu held.
func (e *Engine) saveAverage(a Average, st AverageState) {
	av := &e.averages
	ctx := av.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if av.saving == nil {
		av.saving = map[Average]*AverageState{}
	}
	st.Buckets = append([]AverageBucket(nil), st.Buckets...)
	_, busy := av.saving[a]
	av.saving[a] = &st
	if busy {
		return
	}
	e.goBusy(func() {
		for {
			av.mu.Lock()
			next := av.saving[a]
			if next == nil {
				delete(av.saving, a)
				av.mu.Unlock()
				return
			}
			av.saving[a] = nil
			av.mu.Unlock()
			if err := e.AverageStates.Put(ctx, a.Key(), *next); err != nil && ctx.Err() == nil {
				e.Log.Error("Failed to save average", "err", err, "average", a.Key().Name)
			}
		}
	})
}

// averageOf is the average a condition reads as device's capability name at now, if it's one of the
// engine's and has a reading.
func (e *Engine) averageOf(device, name string, now time.Time) (float64, bool) {
	e.averages.mu.Lock()
	defer e.averages.mu.Unlock()
	for _, a := range e.Averages {
		if a.Device == device && a.Var() == name {
			return e.averageState(a, now).at(a, now)
		}
	}
	return 0, false
}

// addAverages puts each average at now into devices, as its device's capability Var names. A device
// that isn't there, or an average with no reading yet, is left out, as an unreported value is.
func (e *Engine) addAverages(devices map[string]map[string]any, now time.Time) {
	if len(e.Averages) == 0 {
		return
	}
	e.averages.mu.Lock()
	defer e.averages.mu.Unlock()
	for _, a := range e.Averages {
		values, ok := devices[a.Device]
		if !ok {
			continue
		}
		if v, ok := e.averageState(a, now).at(a, now); ok {
			values[a.Var()] = v
		}
	}
}
