//go:build !darwin && !linux

package mdns

import "syscall"

// reusePort does nothing here: Oikade runs on Linux, and is developed on macOS.
func reusePort(_, _ string, _ syscall.RawConn) error { return nil }
