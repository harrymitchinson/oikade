// Package ident names the things a home is made of.
package ident

// ID names a Device, rule or plugin. The host's store keeps saved state under its String form, so
// changing that form loses the state.
type ID struct {
	Name string
}

// String returns the ID's name.
func (id ID) String() string {
	return id.Name
}
