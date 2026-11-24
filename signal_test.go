package oikade

import (
	"strings"
	"testing"
	"time"
)

func TestCheckRefusesASignalsMistakes(t *testing.T) {
	occupied := Bool{Device: "hall-sensor", Capability: Occupied}
	towelRail := Bool{Device: "towel-rail", Capability: On}
	for name, tc := range map[string]struct {
		home func(h *Home)
		want string
	}{
		"a name used twice": {func(h *Home) {
			h.Signal("anyone-home", occupied)
			h.Signal("anyone-home", Not(occupied))
		}, `signal "anyone-home" is made twice`},
		"a name that isn't one": {func(h *Home) { h.Signal("Anyone Home", occupied) }, `"Anyone Home" isn't a signal name`},
		"no name":               {func(h *Home) { h.Signal("", occupied) }, "a signal needs a name"},
		"no condition":          {func(h *Home) { h.Signal("anyone-home") }, "signal anyone-home: it needs a condition"},
		"a condition's mistake": {
			func(h *Home) { h.Signal("summer", Yearly("1 Jun", "31 Jun")) }, `signal summer: Yearly: "31 Jun" isn't a date`,
		},
		"a negative Delay": {
			func(h *Home) { h.Signal("anyone-home", occupied).Delay(-time.Minute) }, "Delay -1m0s must not be negative",
		},
		"a negative Linger": {
			func(h *Home) { h.Signal("anyone-home", occupied).Linger(-time.Hour) }, "Linger -1h0m0s must not be negative",
		},
		"a negative MaxAge": {
			func(h *Home) { h.Signal("anyone-home", occupied).MaxAge(-time.Hour) }, "MaxAge -1h0m0s must not be negative",
		},
		"Linger twice": {
			func(h *Home) { h.Signal("anyone-home", occupied).Linger(time.Hour).Linger(2 * time.Hour) },
			"signal anyone-home: Linger is given twice",
		},
		"a zero Signal in a case": {
			func(h *Home) { h.Decide(towelRail).Case("away", false, Signal{}).Default(true) }, "a zero Signal",
		},
		"a zero Signal in a signal": {func(h *Home) { h.Signal("upstairs", Signal{}) }, "signal upstairs: a zero Signal"},
		"a zero Signal in When": {
			func(h *Home) { h.Flow("arrived", nothing).When(Signal{}.Becomes(true)) }, "the change at When[0]: a zero Signal",
		},
		"another home's Signal in a case": {
			func(h *Home) {
				other := NewHome(Options{}).Signal("anyone-home", occupied)
				h.Decide(towelRail).Case("away", false, Not(other)).Default(true)
			},
			"decision towel-rail on reads signal/anyone-home, which is another home's",
		},
		"another home's Signal in a flow's If": {
			func(h *Home) {
				other := NewHome(Options{}).Signal("anyone-home", occupied)
				h.Flow("arrived", nothing).When(occupied).If(All(occupied, other))
			},
			"flow arrived reads signal/anyone-home, which is another home's",
		},
		"another home's Signal in When": {
			func(h *Home) {
				other := NewHome(Options{}).Signal("anyone-home", occupied)
				h.Flow("arrived", nothing).When(other)
			},
			"the change at When[0]: signal/anyone-home is another home's",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := homeWith(Options{}, tc.home).compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func TestASignalIsNamedByItsRef(t *testing.T) {
	h := NewHome(Options{})
	s := h.Signal("anyone-home", Bool{Device: "hall-sensor", Capability: Occupied})
	if got := s.String(); got != "signal/anyone-home" {
		t.Errorf("String() = %q", got)
	}
	if got := label(Not(s)); got != "not(signal/anyone-home)" {
		t.Errorf("label = %q", got)
	}
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
}
