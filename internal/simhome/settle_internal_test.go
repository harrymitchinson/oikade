package simhome

import (
	"os"
	"testing"
	"time"
)

// Every test here settles only when the engine says it may be idle: one it doesn't tell fails as
// a home that didn't settle, rather than passing a millisecond late each time (OIK-314). On Linux,
// a sleep shorter than that takes about a millisecond, which made this package take 40 seconds.
func TestMain(m *testing.M) {
	settleRecheck = time.Hour
	os.Exit(m.Run())
}
