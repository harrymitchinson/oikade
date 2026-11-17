package simhome_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/internal/simhome"
	"github.com/harrymitchinson/oikade/internal/testkit"
)

// flowSim runs a home with one flow, f, on the sensor's humidity, and sets the sensor off.
func flowSim(t *testing.T, decide func(*oikade.Home), run func(*oikade.FlowRun)) (*simhome.Sim, *testkit.Log) {
	t.Helper()
	home := oikade.NewHome(oikade.Options{})
	home.Flow("f", run).When(humidityOf("sensor"))
	decide(home)
	opts := options(t, home)
	log := testkit.Logs(t)
	opts.Log = log.Logger
	sim, err := simhome.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	if err := sim.Set("sensor", humidity, 50.0); err != nil {
		t.Fatal(err)
	}
	return sim, log
}

func nothingDecided(*oikade.Home) {}

// A Set that fails is logged when it fails, and not again each time a restart replays it, though
// the run still ends as failed.
func TestAFailedStepIsLoggedOnceAcrossARestart(t *testing.T) {
	fan := switchOf("fan")
	sim, log := flowSim(t, func(h *oikade.Home) { h.Decide(fan).Default(false) }, func(r *oikade.FlowRun) {
		r.Set(fan.To(true).For(time.Hour)) // The decision has no Flows line for it.
		r.Sleep(time.Hour)
	})
	if err := sim.Advance(time.Minute); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := sim.Restart(); err != nil {
			t.Fatal(err)
		}
	}
	if err := sim.Advance(2 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := log.Count(`msg="Failed step of flow run"`); got != 1 {
		t.Errorf("logged the failed step %d times", got)
	}
	if sim.Finished("f") || len(sim.Failed("f")) == 0 {
		t.Errorf("the run didn't fail: %v", sim.Failed("f"))
	}
}

// Fail ends a run as failed even if the flow recovers from it.
func TestFailCountsPastARecover(t *testing.T) {
	sim, _ := flowSim(t, nothingDecided, func(r *oikade.FlowRun) {
		func() {
			defer func() { _ = recover() }()
			r.Fail(errors.New("stop here"))
		}()
	})
	if err := sim.Advance(time.Minute); err != nil {
		t.Fatal(err)
	}
	if sim.Finished("f") || len(sim.Failed("f")) == 0 {
		t.Errorf("the run didn't fail: %v", sim.Failed("f"))
	}
}

// A Do whose call fails returns the zero value, as a replay of it would, whatever the call returned.
func TestADoThatFailsReturnsTheZeroValue(t *testing.T) {
	fan := switchOf("fan")
	sim, _ := flowSim(t, nothingDecided, func(r *oikade.FlowRun) {
		n, err := r.Do("count", func(context.Context) (int, error) { return 3, errors.New("no answer") })
		if err != nil && n == 0 {
			r.Set(fan.To(true))
		}
	})
	if err := sim.Advance(time.Minute); err != nil {
		t.Fatal(err)
	}
	if !sim.Finished("f") {
		t.Fatalf("the run didn't finish: %v", sim.Failed("f"))
	}
	if v, _ := sim.Value("fan", on); v != true {
		t.Errorf("fan is %v", v)
	}
}

// A flow step that never ends keeps the home from settling, which says so rather than waiting for
// good, and the home still stops.
func TestAStuckStepIsAHomeThatDidntSettle(t *testing.T) {
	home := oikade.NewHome(oikade.Options{})
	home.Flow("stuck", func(r *oikade.FlowRun) {
		_, _ = r.Do("the weather", func(ctx context.Context) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		})
	}).When(humidityOf("sensor"))
	opts := options(t, home)
	opts.SettleTimeout = 100 * time.Millisecond
	sim, err := simhome.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	err = sim.Set("sensor", humidity, 50.0)
	if want := "the home didn't settle: a condition or flow step may be stuck"; err == nil || err.Error() != want {
		t.Fatalf("Set = %v, want %q", err, want)
	}
}
