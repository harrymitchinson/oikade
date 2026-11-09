package rules

import "github.com/harrymitchinson/oikade/framework"

type other struct{}

func (o other) Chain() other { return o }

func used() framework.Hold {
	h := framework.NewHold("a")
	h = h.Latch()
	_ = h.Latch()
	return h.Latch()
}

func discarded(s framework.Set, g framework.Handle[bool], r *framework.FlowRun) {
	h := framework.NewHold("a")
	h.Latch()         // want `the result of Latch isn't used, so it does nothing: Hold's methods return a new value`
	(h.Latch())       // want `the result of Latch isn't used`
	s.UntilNext()     // want `the result of UntilNext isn't used, so it does nothing: Set's methods`
	g.Also()          // want `the result of Also isn't used`
	h.Latch().Latch() // want `the result of Latch isn't used`

	// Not chained: another type back, or not the framework's.
	h.Name()
	g.To(true)
	_ = r.Set(s)
	r.Set(s)
	other{}.Chain()
}
