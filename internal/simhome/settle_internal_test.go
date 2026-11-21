package simhome

import (
	"os"
	"testing"
	"time"
)

// Every test here settles only when the engine says it may be idle: one it doesn't tell fails as
// a home that didn't settle, rather than passing a millisecond late each time. It keeps the package
// fast too: on Linux, each recheck's sleep takes about a millisecond, however short it's asked for.
func TestMain(m *testing.M) {
	settleRecheck = time.Hour
	os.Exit(m.Run())
}
