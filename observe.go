package oikade

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"oikade.dev/internal/engine"
	"oikade.dev/internal/flow"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/ident"
)

const (
	// metricsEvery is how often the host hears the process's rule metrics, which it serves itself.
	metricsEvery = 5 * time.Second
	// runsWaiting bounds the rule runs waiting to reach the host. oikade tail misses any beyond it.
	runsWaiting = 256
)

type ruleRun struct {
	rule, result string
	at           time.Time
}

// observe gives eng its metrics, and returns what reports them and its runs to the host.
func observe(ctx context.Context, eng *engine.Engine, client *wire.Client) func() {
	set := metrics.NewSet()
	eng.RuleMetrics = set

	runs := make(chan ruleRun, runsWaiting)
	eng.OnRun = func(key ident.ID, result string) {
		if result == engine.ResultConditionFalse {
			return // Every change to anything a condition reads. Too much to watch.
		}
		select {
		case runs <- ruleRun{key.Name, result, eng.Clock.Now()}:
		default:
		}
	}
	// reads keeps each hold's latest room reads until they're sent, so none is dropped. readsMu
	// guards stopped too, and readsWaiting says either has some.
	var readsMu sync.Mutex
	reads := map[string][]engine.RoomRead{}
	readsWaiting := make(chan struct{}, 1)
	eng.OnRoomReads = func(key ident.ID, r []engine.RoomRead) {
		readsMu.Lock()
		reads[key.Name] = r
		readsMu.Unlock()
		select {
		case readsWaiting <- struct{}{}:
		default:
		}
	}
	// stopped keeps each hold's latest reason until it's sent, so a clearing one isn't dropped.
	stopped := map[string]string{}
	eng.OnStopped = func(key ident.ID, why string) {
		readsMu.Lock()
		stopped[key.Name] = why
		readsMu.Unlock()
		select {
		case readsWaiting <- struct{}{}:
		default:
		}
	}
	// whyNot keeps why each case is inactive until it's sent, as stopped does.
	whyNot := map[string]string{}
	eng.OnWhyNot = func(key ident.ID, why string) {
		readsMu.Lock()
		whyNot[key.Name] = why
		readsMu.Unlock()
		select {
		case readsWaiting <- struct{}{}:
		default:
		}
	}
	// signalWhy keeps what each signal last said until it's sent, as whyNot does.
	signalWhy := map[string]wire.Message{}
	eng.OnSignalWhy = func(name, why string, dead []string) {
		readsMu.Lock()
		signalWhy[name] = wire.Message{Why: why, Dead: dead}
		readsMu.Unlock()
		select {
		case readsWaiting <- struct{}{}:
		default:
		}
	}
	return func() {
		tick := time.NewTicker(metricsEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-client.Done():
				return
			case r := <-runs:
				client.RuleRun(r.rule, r.result, r.at)
			case <-readsWaiting:
				readsMu.Lock()
				waiting, why, not, said := reads, stopped, whyNot, signalWhy
				reads, stopped, whyNot = map[string][]engine.RoomRead{}, map[string]string{}, map[string]string{}
				signalWhy = map[string]wire.Message{}
				readsMu.Unlock()
				for _, rule := range slices.Sorted(maps.Keys(waiting)) {
					client.RoomReads(rule, waiting[rule])
				}
				for _, rule := range slices.Sorted(maps.Keys(why)) {
					client.Stopped(rule, why[rule])
				}
				for _, rule := range slices.Sorted(maps.Keys(not)) {
					client.WhyNot(rule, not[rule])
				}
				for _, name := range slices.Sorted(maps.Keys(said)) {
					client.SignalWhy(name, said[name].Why, said[name].Dead)
				}
			case <-tick.C:
				var text bytes.Buffer
				set.WritePrometheus(&text)
				client.PushMetrics(text.Bytes())
			}
		}
	}
}

// pauses keeps paused rules out of the engine. The host says when that changes, and the engine works
// apart from the read goroutine that hears it.
type pauses struct {
	eng     *engine.Engine
	client  *wire.Client
	rules   map[string]*engine.Compiled
	changes chan pauseChange

	mu     sync.Mutex
	paused map[string]bool
}

type pauseChange struct {
	rule   string
	paused bool
}

func pausedRules(eng *engine.Engine, client *wire.Client, rules []*engine.Compiled, names []string) *pauses {
	p := &pauses{
		eng: eng, client: client, rules: map[string]*engine.Compiled{}, paused: map[string]bool{},
		changes: make(chan pauseChange, 64),
	}
	for _, r := range rules {
		p.rules[r.Key.Name] = r
	}
	for _, name := range names {
		p.paused[name] = true
	}
	return p
}

func (p *pauses) has(rule string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused[rule]
}

// without returns flows but for the paused ones.
func (p *pauses) without(flows map[string]*flow.Flow) map[string]*flow.Flow {
	left := maps.Clone(flows)
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, paused := range p.paused {
		if paused {
			delete(left, name)
		}
	}
	return left
}

// change hears the host. It can't block, and a process that can't keep up starts again from a Welcome.
func (p *pauses) change(rule string, paused bool) {
	select {
	case p.changes <- pauseChange{rule, paused}:
	default:
		p.client.Close()
	}
}

func (p *pauses) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.client.Done():
			return
		case c := <-p.changes:
			p.apply(ctx, c)
		}
	}
}

func (p *pauses) apply(ctx context.Context, c pauseChange) {
	p.mu.Lock()
	was := p.paused[c.rule]
	p.paused[c.rule] = c.paused
	p.mu.Unlock()
	r := p.rules[c.rule]
	if r == nil || was == c.paused {
		return
	}
	if c.paused {
		p.eng.Unload(r.Key)
		// The host did this too. Doing it after the run stopped ends what that run claimed since. A
		// failure here ends the session, and the next process starts paused.
		_ = p.eng.ReleaseFlow(ctx, r.Key)
		return
	}
	if r.HasTimers() {
		// A time that passed during a pause is one you didn't want, now and after any restart. A
		// failed save ends the session or leaves the time to catch up once, which actions can stand.
		now := p.eng.Clock.Now()
		r.KeepSince(now)
		_ = p.client.SaveFire(ctx, c.rule, now)
	}
	p.eng.Load(r)
}
