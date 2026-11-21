package control

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/clock"
)

// TestArbiterAgreesWithAModel makes random claims, releases, outside changes and clock moves, and
// checks each decision against a model of the claims written apart from the arbiter, against the
// limits, and against a restart from its State. A failure prints the settings and the sequence that
// led to it.
func TestArbiterAgreesWithAModel(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for seq := range 3000 {
		s := randomSettings(r)
		c := clock.NewFake(at("10:00"))
		a, err := New(s, c, State{})
		if err != nil {
			t.Fatal(err)
		}
		p := &property{t: t, seq: seq, s: s, a: a, c: c, m: &model{s: s}}
		for range 40 {
			p.step(r)
		}
	}
}

var propertySources = []string{"Rule/a", "Rule/b", "Rule/c", "Rule/d"}

func randomSettings(r *rand.Rand) Settings {
	s := Settings{Kind: Bool, External: ExternalOverride, Location: london}
	if r.IntN(4) == 0 {
		s.External = ExternalRevert
	}
	if r.IntN(2) == 0 {
		s.OverrideUntilNext = true
	} else {
		s.OverrideHold = time.Duration(1+r.IntN(120)) * time.Minute
	}
	if r.IntN(4) == 0 {
		s.Default = 1
	}
	if r.IntN(2) == 0 {
		s.MinimumOn = time.Duration(r.IntN(60)) * time.Minute
	}
	if r.IntN(2) == 0 {
		s.MinimumOff = time.Duration(r.IntN(60)) * time.Minute
	}
	if r.IntN(3) == 0 {
		s.MaximumOn = s.MinimumOn + time.Duration(1+r.IntN(180))*time.Minute
	}
	if r.IntN(3) == 0 {
		s.MaximumStartsPerDay = 1 + r.IntN(3)
	}
	switch r.IntN(5) {
	case 0:
		s.Lines = []string{"Rule/a", LineOverride, "Rule/b", LineFlows}
	case 1:
		s.Lines = []string{LineFlows, "Rule/c", LineOverride, "Rule/a"}
	case 2:
		// No flows line: only the lines may claim.
		s.Lines = []string{"Rule/b", LineOverride, "Rule/a"}
	case 3:
		// No override line either: an outside change ranks after every line.
		s.Lines = []string{"Rule/a", "Rule/b"}
	}
	return s
}

// model is the claims as the package doc describes them, kept without the arbiter's code.
type model struct {
	s Settings
	// claims are live, in the order they came.
	claims []Claim
}

// rank is a claim's line, from 0 at the top. A line the decision doesn't have is after them all.
func (m *model) rank(c Claim) int {
	line := c.Source
	if c.External {
		line = LineOverride
	} else if !slices.Contains(m.s.Lines, c.Source) {
		line = LineFlows
	}
	if i := slices.Index(m.s.Lines, line); i >= 0 {
		return i
	}
	return len(m.s.Lines)
}

// sorted are the claims as a decision lists them: highest level first, then by line, then the
// latest first.
func (m *model) sorted(claims []Claim) []Claim {
	type at struct {
		c Claim
		i int
	}
	out := make([]at, len(claims))
	for i, c := range claims {
		out[i] = at{c, i}
	}
	slices.SortFunc(out, func(x, y at) int {
		switch {
		case x.c.Level != y.c.Level:
			return int(y.c.Level - x.c.Level)
		case m.rank(x.c) != m.rank(y.c):
			return m.rank(x.c) - m.rank(y.c)
		}
		return y.i - x.i
	})
	cs := make([]Claim, len(out))
	for i, o := range out {
		cs[i] = o.c
	}
	return cs
}

// winner is the claim that wins among claims, or the default.
func (m *model) winner(claims []Claim) (float64, Level, string) {
	if len(claims) == 0 {
		return m.s.Default, LevelDefault, ""
	}
	w := m.sorted(claims)[0]
	return w.Value, w.Level, w.Source
}

// settle ends expired claims, and until-next claims whose result without them has moved off their
// baseline, until there are none to end.
func (m *model) settle(now time.Time) {
	for {
		i := slices.IndexFunc(m.claims, func(c Claim) bool { return !c.Expires.IsZero() && !now.Before(c.Expires) })
		for j := 0; i < 0 && j < len(m.claims); j++ {
			if c := m.claims[j]; c.UntilNext {
				if v, _, _ := m.winner(slices.Delete(slices.Clone(m.claims), j, j+1)); v != c.Baseline {
					i = j
				}
			}
		}
		if i < 0 {
			return
		}
		m.claims = slices.Delete(m.claims, i, i+1)
	}
}

func (m *model) drop(source string) {
	m.claims = slices.DeleteFunc(m.claims, func(c Claim) bool { return c.Source == source })
}

// add is a claim from a source, replacing its last, on the result without it.
func (m *model) add(c Claim, now time.Time) {
	m.drop(c.Source)
	m.settle(now)
	c.Baseline, _, _ = m.winner(m.claims)
	m.claims = append(m.claims, c)
	m.settle(now)
}

// refuses says whether a claim from source has no line to be on.
func (m *model) refuses(source string) bool {
	return len(m.s.Lines) > 0 && !slices.Contains(m.s.Lines, source) && !slices.Contains(m.s.Lines, LineFlows)
}

type property struct {
	t   *testing.T
	seq int
	s   Settings
	a   *Arbiter
	c   *clock.Fake
	m   *model
	log []string
	dev device
}

// device is what the device has, kept apart from the arbiter: its value, when it last turned on
// and off, its starts on the local day, and an outside change waiting to be written back.
type device struct {
	known, on       bool
	lastOn, lastOff time.Time
	day             time.Time
	starts          int
	revert          *Revert
}

// localDay is the start of t's day where the settings are.
func (p *property) localDay(t time.Time) time.Time {
	y, m, d := t.In(p.s.Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, p.s.Location)
}

// record is the device turning to v, counting a start if it was known to be off.
func (p *property) record(v float64) {
	now := p.c.Now()
	if d := p.localDay(now); !d.Equal(p.dev.day) {
		p.dev.day, p.dev.starts = d, 0
	}
	switch on := v != 0; {
	case on && (!p.dev.known || !p.dev.on):
		p.dev.lastOn = now
		if p.dev.known {
			p.dev.starts++
		}
	case !on && p.dev.known && p.dev.on:
		p.dev.lastOff = now
	}
	p.dev.known, p.dev.on = true, v != 0
}

func (p *property) value() float64 {
	if p.dev.on {
		return 1
	}
	return 0
}

func (p *property) fail(format string, args ...any) {
	p.t.Helper()
	p.t.Fatalf("sequence %d: %s\nsettings %+v\nops:\n  %s", p.seq, fmt.Sprintf(format, args...), p.s, strings.Join(p.log, "\n  "))
}

func (p *property) note(format string, args ...any) {
	p.log = append(p.log, p.c.Now().Format("15:04 ")+fmt.Sprintf(format, args...))
}

func (p *property) step(r *rand.Rand) {
	p.t.Helper()
	switch op := r.IntN(10); {
	case op < 4:
		p.claim(r)
	case op < 6:
		src := propertySources[r.IntN(len(propertySources))]
		p.note("release %s", src)
		p.a.Release(src)
		p.m.drop(src)
		p.m.settle(p.c.Now())
	case op < 7:
		// Two outside sources, so one's change has to replace the other's.
		p.outside([]string{"Device", "Matter"}[r.IntN(2)], float64(r.IntN(2)))
	default:
		p.c.Step(time.Duration(1+r.IntN(90)) * time.Minute)
		p.note("(clock)")
	}
	p.decide()
}

func (p *property) claim(r *rand.Rand) {
	p.t.Helper()
	now := p.c.Now()
	cl := Claim{Source: propertySources[r.IntN(len(propertySources))], Level: Level(1 + r.IntN(3)), Value: float64(r.IntN(2))}
	if r.IntN(3) == 0 {
		cl.Expires = now.Add(time.Duration(1+r.IntN(180)) * time.Minute)
	}
	if r.IntN(4) == 0 {
		cl.UntilNext = true
	}
	if r.IntN(8) == 0 {
		// The arbiter sets these, whatever a claim says.
		cl.External, cl.Baseline = true, 1
	}
	p.note("claim %+v", cl)
	err := p.a.Claim(cl)
	if refused := p.m.refuses(cl.Source); refused != (err != nil) {
		p.fail("claim refused %v, want refused %v: %v", err != nil, refused, err)
	}
	if err == nil {
		cl.External, cl.Baseline = false, 0
		p.m.add(cl, now)
	}
}

func (p *property) outside(source string, v float64) {
	p.t.Helper()
	p.note("outside %s %v", source, v)
	if err := p.a.Outside(source, v); err != nil {
		p.fail("outside refused: %v", err)
	}
	if p.dev.known && v == p.value() {
		// A value the device already has is no change.
		return
	}
	if p.s.External == ExternalRevert && p.dev.known {
		r := &Revert{Source: source, Value: v, From: p.value()}
		if p.dev.revert != nil {
			r.From = p.dev.revert.From
		}
		p.dev.revert, p.dev.on = r, v != 0
		return
	}
	p.record(v)
	if p.s.External == ExternalRevert {
		return
	}
	now := p.c.Now()
	p.m.claims = slices.DeleteFunc(p.m.claims, func(c Claim) bool { return c.External })
	c := Claim{Source: source, Level: Override, Value: v, External: true, UntilNext: p.s.OverrideUntilNext}
	if !c.UntilNext {
		c.Expires = now.Add(p.s.OverrideHold)
	}
	p.m.add(c, now)
}

func (p *property) decide() {
	p.t.Helper()
	now := p.c.Now()
	d := p.a.Decide()
	p.m.settle(now)

	// The claims and their winner are the model's.
	want := p.m.sorted(p.m.claims)
	if !slices.EqualFunc(d.Claims, want, sameClaim) {
		p.fail("claims\n  %+v\nwant\n  %+v", d.Claims, want)
	}
	if v, l, src := p.m.winner(p.m.claims); d.Wanted != v || d.Level != l || d.Source != src {
		p.fail("winner %v %q wanting %v, want %v %q wanting %v", d.Level, d.Source, d.Wanted, l, src, v)
	}
	// The decision looks again no later than the first claim ends.
	if !d.Next.IsZero() && !d.Next.After(now) {
		p.fail("next %v isn't after now %v", d.Next, now)
	}
	for _, c := range want {
		if !c.Expires.IsZero() && (d.Next.IsZero() || d.Next.After(c.Expires)) {
			p.fail("next %v is after %+v ends", d.Next, c)
		}
	}

	// An outside change the winner has moved on from stands, and counts.
	if r := p.dev.revert; r != nil && d.Wanted != r.From {
		p.dev.revert, p.dev.on = nil, r.From != 0
		p.record(r.Value)
	}
	if p.dev.revert != nil {
		// Writing the earlier value back is exempt from the limits.
		if d.Value != d.Wanted || d.Limit != NoLimit {
			p.fail("a revert writes %v (limit %v), not the winner's %v", d.Value, d.Limit, d.Wanted)
		}
	} else {
		p.checkLimits(d, now)
	}
	if d.Write != (!p.dev.known || d.Value != p.value()) {
		p.fail("write %v, with the device known %v at %v and the decision %v", d.Write, p.dev.known, p.value(), d.Value)
	}

	// A restart from the saved state, through JSON as the store keeps it, decides the same.
	raw, err := json.Marshal(p.a.State())
	if err != nil {
		p.fail("state doesn't marshal: %v", err)
	}
	var restored State
	if err := json.Unmarshal(raw, &restored); err != nil {
		p.fail("state doesn't unmarshal: %v", err)
	}
	again, err := New(p.s, p.c, restored)
	if err != nil {
		p.fail("restored state refused: %v", err)
	}
	if d2 := again.Decide(); !sameDecision(d, d2) {
		p.fail("after a restart the decision is\n  %+v\nand was\n  %+v", d2, d)
	}

	if d.Write {
		p.a.Wrote(d.Value)
		p.note("wrote %v (%v %q, limit %v)", d.Value, d.Level, d.Source, d.Limit)
		if r := p.dev.revert; r != nil {
			p.dev.revert = nil
			if d.Value == r.From {
				// The write back undoes the change, so neither counts.
				p.dev.on = d.Value != 0
				r = nil
			}
			if r == nil {
				p.again()
				return
			}
		}
		p.record(d.Value)
		p.again()
	}
}

// again checks that, having written, there's nothing more to write at the same moment.
func (p *property) again() {
	p.t.Helper()
	if d := p.a.Decide(); d.Write {
		p.fail("wants to write %v straight after a write", d.Value)
	}
}

// checkLimits checks the limits against the device: a limit holds the winner back only while it
// applies, and a value it lets through breaks none.
func (p *property) checkLimits(d Decision, now time.Time) {
	p.t.Helper()
	if d.Limit == NoLimit {
		if d.Value != d.Wanted {
			p.fail("no limit, but value %v isn't the wanted %v", d.Value, d.Wanted)
		}
	} else if d.Value == d.Wanted {
		p.fail("limit %v, but the value is the wanted %v", d.Limit, d.Wanted)
	}
	if !p.dev.known {
		if d.Limit != NoLimit {
			p.fail("limit %v on a device whose value isn't known", d.Limit)
		}
		return
	}
	if broken := p.brokenLimit(d, now); broken != "" {
		p.fail("%s", broken)
	}
}

// brokenLimit says how a decision on a device whose value is known gets a limit wrong, if it does.
func (p *property) brokenLimit(d Decision, now time.Time) string {
	s, dev := p.s, p.dev
	starts := dev.starts
	if !p.localDay(now).Equal(dev.day) {
		starts = 0
	}
	inMinimumOn := now.Before(dev.lastOn.Add(s.MinimumOn))
	inMinimumOff := now.Before(dev.lastOff.Add(s.MinimumOff))
	pastMaximumOn := s.MaximumOn > 0 && !now.Before(dev.lastOn.Add(s.MaximumOn))
	outOfStarts := s.MaximumStartsPerDay > 0 && starts >= s.MaximumStartsPerDay
	switch {
	case d.Limit == MinimumOn && (!dev.on || !inMinimumOn):
		return fmt.Sprintf("held on by MinimumOn %v after it turned on", now.Sub(dev.lastOn))
	case d.Limit == MinimumOff && (dev.on || !inMinimumOff):
		return fmt.Sprintf("held off by MinimumOff %v after it turned off", now.Sub(dev.lastOff))
	case d.Limit == MaximumStartsPerDay && (dev.on || !outOfStarts):
		return fmt.Sprintf("held off by MaximumStartsPerDay after %d starts today", starts)
	case d.Limit == MaximumOn && dev.on && !pastMaximumOn:
		return fmt.Sprintf("stopped by MaximumOn %v after it turned on", now.Sub(dev.lastOn))
	case dev.on && d.Value == 0 && inMinimumOn:
		return fmt.Sprintf("turned off %v after turning on, inside MinimumOn", now.Sub(dev.lastOn))
	case dev.on && d.Value != 0 && pastMaximumOn:
		return fmt.Sprintf("kept on %v, past MaximumOn", now.Sub(dev.lastOn))
	case !dev.on && d.Value != 0 && inMinimumOff:
		return fmt.Sprintf("turned on %v after turning off, inside MinimumOff", now.Sub(dev.lastOff))
	case !dev.on && d.Value != 0 && outOfStarts:
		return fmt.Sprintf("start %d today, over MaximumStartsPerDay", starts+1)
	}
	return ""
}

func sameClaim(a, b Claim) bool {
	return a.Source == b.Source && a.Level == b.Level && a.Value == b.Value && a.Expires.Equal(b.Expires) &&
		a.UntilNext == b.UntilNext && a.Baseline == b.Baseline && a.External == b.External
}

func sameDecision(a, b Decision) bool {
	return a.Value == b.Value && a.Write == b.Write && a.Wanted == b.Wanted && a.Level == b.Level &&
		a.Source == b.Source && a.Limit == b.Limit && a.Next.Equal(b.Next) && slices.EqualFunc(a.Claims, b.Claims, sameClaim)
}
