package hometest_test

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade"
	"github.com/harrymitchinson/oikade/hometest"
)

// home is the home the examples test, with the devices devices_test.go declares, as oikade types
// writes them: the porch light from oikade init, a flow that turns the lounge off at bedtime, and a
// hall fan for a steamy, occupied hall.
func home() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(PorchLight.On).
		Case("motion", true, PorchSensor.Occupied).Linger(5 * time.Minute).
		Default(false)
	home.Flow("bedtime", func(r *oikade.FlowRun) {
		r.Sleep(30 * time.Minute)
		r.Set(Lounge.On.To(false))
	}).At(oikade.Clock("22:30"))
	home.Decide(HallFan.On).
		Case("steamy", true, Hall.Humidity.Above(65), Hall.Occupied).
		Default(false)
	return home
}

// t stands in, in the examples, for the *testing.T a test is given. An Expect that fails panics with
// its message, which fails the example.
var t testing.TB = &exampleT{}

type exampleT struct {
	testing.TB
	cleanups []func()
}

func (*exampleT) Helper()                        {}
func (*exampleT) Log(...any)                     {}
func (e *exampleT) Cleanup(f func())             { e.cleanups = append(e.cleanups, f) }
func (*exampleT) Fatal(args ...any)              { panic(fmt.Sprint(args...)) }
func (*exampleT) Fatalf(format string, a ...any) { panic(fmt.Sprintf(format, a...)) }

// TestMain stops the examples' homes once every test and example has run.
func TestMain(m *testing.M) {
	code := m.Run()
	e := t.(*exampleT)
	for _, v := range slices.Backward(e.cleanups) {
		v()
	}
	os.Exit(code)
}

// A test sets a device as its sensor would, moves the clock on and expects what the home does.
func Example() {
	h := hometest.New(t, home(), hometest.Options{Types: "devices_test.go"})

	h.Set(PorchSensor.Occupied, true)
	h.ExpectValue(PorchLight.On, true)

	h.Set(PorchSensor.Occupied, false)
	h.Advance(4 * time.Minute)
	h.ExpectValue(PorchLight.On, true)

	h.Advance(2 * time.Minute)
	h.ExpectValue(PorchLight.On, false)

	fmt.Println(h.Value(PorchLight.On))
	// Output: false true
}

// A restart in the middle of a flow is one call, and the flow carries on from where it had got to.
func ExampleHome_Restart() {
	h := hometest.New(t, home(), hometest.Options{Types: "devices_test.go"})
	h.Advance(22*time.Hour + 40*time.Minute) // Into the flow's 30-minute wait.
	h.Restart()
	h.Advance(21 * time.Minute)
	h.ExpectValue(Lounge.On, false)
	h.ExpectFinished("bedtime")

	fmt.Println(h.Now().Format("15:04"), h.Finished("bedtime"))
	// Output: 23:01 true
}

// A report sends several of one device's values at once, and the home sees all of them before
// anything acts.
func ExampleHome_Report() {
	h := hometest.New(t, home(), hometest.Options{Types: "devices_test.go"})
	h.Report(Hall.Humidity.To(72), Hall.Occupied.To(true))
	h.ExpectValue(HallFan.On, true)

	fmt.Println(h.Value(HallFan.On))
	// Output: true true
}
