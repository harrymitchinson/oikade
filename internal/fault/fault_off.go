//go:build !faultpoints

package fault

// Hit is a fault point. It does nothing in a normal build.
func Hit(string) {}

// Drop reports whether a fault point should skip its work. It's always false in a normal build.
func Drop(string) bool { return false }
