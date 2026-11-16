package rules

import "github.com/harrymitchinson/oikade"

type other struct{}

func (o other) Chain() other { return o }

func used(s oikade.Set, g oikade.Handle[bool], r *oikade.FlowRun) oikade.Home {
	h := oikade.NewHold("a", s)
	h = h.Latch()
	_ = h.Latch()
	_ = r.Set(g.To(true))
	_ = oikade.NewBundle(h)
	return oikade.NewHome(h, oikade.NewBundle(h.Latch()))
}

func discarded(s oikade.Set, g oikade.Handle[bool], r *oikade.FlowRun) {
	h := oikade.NewHold("a", s)
	h.Latch()         // want `the result of Latch isn't used, so it does nothing: Hold's methods return a new value`
	(h.Latch())       // want `the result of Latch isn't used`
	s.UntilNext()     // want `the result of UntilNext isn't used, so it does nothing: Set's methods`
	h.Latch().Latch() // want `the result of Latch isn't used`

	defer h.Latch()  // want `the result of Latch isn't used`
	go s.UntilNext() // want `the result of UntilNext isn't used`
	h.
		Latch().
		Latch() // want `the result of Latch isn't used`
}

func thrownAway(s oikade.Set, g oikade.Handle[bool], r *oikade.FlowRun) {
	g.To(true)                           // want `the result of To isn't used, so it does nothing: a Set does nothing until r.Set or a rule takes it`
	oikade.InRoom("hall", g).To(true) // want `the result of To isn't used, so it does nothing: a Set`
	oikade.InRoom("hall", g)          // want `the result of InRoom isn't used, so it does nothing: a Room does nothing until something takes it`
	oikade.NewHold("a", s)            // want `the result of NewHold isn't used, so it does nothing: a Hold does nothing until NewHome or NewBundle takes it`
	oikade.NewBundle()                // want `the result of NewBundle isn't used, so it does nothing: a Bundle does nothing`
	oikade.NewHome()                  // want `the result of NewHome isn't used, so it does nothing: a Home does nothing until oikade.Run takes it`
	oikade.NewWindow()                // want `the result of NewWindow isn't used`
	defer g.To(true)                     // want `the result of To isn't used`
}

// Results that aren't oikade values, or calls that do something.
func notFlagged(s oikade.Set, g oikade.Handle[bool], r *oikade.FlowRun, st oikade.State) {
	r.Set(s)
	r.WaitFor()
	r.Release()
	st.Get(g)
	oikade.Run(oikade.NewHome())
	oikade.Secret("token")
	other{}.Chain()
	// A method value isn't followed: a gap, not a wrong report.
	f := g.To
	f(true)
}
