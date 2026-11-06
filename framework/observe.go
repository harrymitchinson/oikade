package framework

import (
	"bytes"
	"context"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/wire"
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
	log := logr.FromContextOrDiscard(ctx)
	eng.Runs, eng.NextChange = engine.NewRunsCounter(), engine.NewNextChangeGauge()
	eng.Dropped, eng.Active, eng.Budget = engine.NewDroppedCounter(), engine.NewActiveGauge(), engine.NewBudgetMetrics()
	registry := prometheus.NewRegistry()
	registry.MustRegister(eng.Runs, eng.NextChange, eng.Dropped, eng.Active)
	registry.MustRegister(eng.Budget.Collectors()...)

	runs := make(chan ruleRun, runsWaiting)
	eng.OnRun = func(key types.NamespacedName, result string) {
		if result == engine.ResultConditionFalse {
			return // Every change to anything a condition reads. Too much to watch.
		}
		select {
		case runs <- ruleRun{key.Name, result, eng.Clock.Now()}:
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
			case <-tick.C:
				text, err := metricsText(registry)
				if err != nil {
					log.Error(err, "Failed to gather metrics")
					continue
				}
				client.PushMetrics(text)
			}
		}
	}
}

func metricsText(g prometheus.Gatherer) ([]byte, error) {
	families, err := g.Gather()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, f := range families {
		if _, err := expfmt.MetricFamilyToText(&out, f); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
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
		r.KeepSince(p.eng.Clock.Now()) // A time that passed during a pause is one you didn't want.
	}
	p.eng.Load(r)
}
