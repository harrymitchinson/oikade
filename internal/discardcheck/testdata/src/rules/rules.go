package rules

import "oikade.dev"

type other struct{}

func (o other) Chain() other { return o }

func used(s oikade.Set, g oikade.Bool, r *oikade.FlowRun) *oikade.Home {
	s = s.UntilNext()
	_ = s.UntilNext()
	r.Set(g.To(true).For(1))
	return oikade.NewHome()
}

func discarded(s oikade.Set, g oikade.Bool, r *oikade.FlowRun) {
	s.UntilNext()        // want `the result of UntilNext isn't used, so it does nothing: Set's methods return a new value`
	(s.UntilNext())      // want `the result of UntilNext isn't used`
	s.UntilNext().For(1) // want `the result of For isn't used`
	defer s.UntilNext()  // want `the result of UntilNext isn't used`
	go s.UntilNext()     // want `the result of UntilNext isn't used`
	s.
		UntilNext().
		UntilNext() // want `the result of UntilNext isn't used`
}

func thrownAway(s oikade.Set, g oikade.Bool, n oikade.Number, r *oikade.FlowRun, h *oikade.Home) {
	g.To(true)                     // want `the result of To isn't used, so it does nothing: a Set does nothing until r.Set takes it`
	h.Room("hall").On.To(true)     // want `the result of To isn't used, so it does nothing: a Set`
	h.Room("hall")                 // want `the result of Room isn't used, so it does nothing: a Group does nothing until something takes it`
	oikade.NewHome()               // want `the result of NewHome isn't used, so it does nothing: a Home does nothing until its Run runs it`
	oikade.Daily("18:00", "22:00") // want `the result of Daily isn't used, so it does nothing: a condition does nothing until something takes it`
	n.Above(3)                     // want `the result of Above isn't used, so it does nothing: a condition does nothing until something takes it`
	oikade.Not(g)                  // want `the result of Not isn't used`
	g.Becomes(true)                // want `the result of Becomes isn't used, so it does nothing: a Change does nothing until something takes it`
	defer g.To(true)               // want `the result of To isn't used`
}

// Results that aren't oikade values, or calls that do something.
func notFlagged(s oikade.Set, g oikade.Bool, r *oikade.FlowRun, st oikade.Snapshot) {
	// A decision, a flow, a plugin and a declared device are the home's as they're built.
	home := oikade.NewHome()
	home.Device("porch-light").Hardware("zigbee", "0x1").Room("Porch")
	home.Decide(g).Case("a", true, g).Default(false)
	home.Flow("a", func(r *oikade.FlowRun) { r.Set(s) }).When(g.Becomes(true))
	home.Plugin("a", nil)
	home.Plugin("b", nil).HideFromMatter()
	r.Set(s)
	r.WaitFor()
	r.Release("a")
	r.Do("call")
	st.Get(g)
	home.Run()
	oikade.Var("TOKEN")
	other{}.Chain()
	// A method value isn't followed: a gap, not a wrong report.
	f := g.To
	f(true)
}
