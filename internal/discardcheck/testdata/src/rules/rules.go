package rules

import "github.com/harrymitchinson/oikade/framework"

type other struct{}

func (o other) Chain() other { return o }

func used(s framework.Set, g framework.Handle[bool], r *framework.FlowRun) framework.Home {
	h := framework.NewHold("a", s)
	h = h.Latch()
	_ = h.Latch()
	_ = r.Set(g.To(true))
	_ = framework.NewBundle(h)
	return framework.NewHome(h, framework.NewBundle(h.Latch()))
}

func discarded(s framework.Set, g framework.Handle[bool], r *framework.FlowRun) {
	h := framework.NewHold("a", s)
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

func thrownAway(s framework.Set, g framework.Handle[bool], r *framework.FlowRun) {
	g.To(true)                           // want `the result of To isn't used, so it does nothing: a Set does nothing until r.Set or a rule takes it`
	framework.InRoom("hall", g).To(true) // want `the result of To isn't used, so it does nothing: a Set`
	framework.InRoom("hall", g)          // want `the result of InRoom isn't used, so it does nothing: a Room does nothing until something takes it`
	framework.NewHold("a", s)            // want `the result of NewHold isn't used, so it does nothing: a Hold does nothing until NewHome or NewBundle takes it`
	framework.NewBundle()                // want `the result of NewBundle isn't used, so it does nothing: a Bundle does nothing`
	framework.NewHome()                  // want `the result of NewHome isn't used, so it does nothing: a Home does nothing until framework.Run takes it`
	framework.NewWindow()                // want `the result of NewWindow isn't used`
	defer g.To(true)                     // want `the result of To isn't used`
}

// Results that aren't framework values, or calls that do something.
func notFlagged(s framework.Set, g framework.Handle[bool], r *framework.FlowRun, st framework.State) {
	r.Set(s)
	r.WaitFor()
	r.Release()
	st.Get(g)
	framework.Run(framework.NewHome())
	framework.Secret("token")
	other{}.Chain()
	// A method value isn't followed: a gap, not a wrong report.
	f := g.To
	f(true)
}
