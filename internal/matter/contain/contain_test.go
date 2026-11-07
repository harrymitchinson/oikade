package contain

import (
	"testing"

	"github.com/harrymitchinson/oikade/internal/logging"
)

func TestRecoverContainsAPanicAndEndsWhatItLeft(t *testing.T) {
	var heard string
	ended := false
	func() {
		defer Recover(logging.Test(t), Handler, func(site string) { heard = site }, func() { ended = true })
		panic("a bug")
	}()
	if heard != Handler || !ended {
		t.Errorf("heard %q, ended %v", heard, ended)
	}
}

// A commissioning command that panics would leave a window or a fail-safe half moved: that has to stop
// the host, with Recover further up.
func TestACriticalPanicGetsPastRecover(t *testing.T) {
	heard := false
	defer func() {
		if r := recover(); r == nil {
			t.Error("Recover contained a critical panic")
		}
		if heard {
			t.Error("a critical panic was counted as contained")
		}
	}()
	func() {
		defer Recover(logging.Test(t), Handler, func(string) { heard = true }, nil)
		func() {
			defer Critical()
			panic("a bug in a commissioning command")
		}()
	}()
}
