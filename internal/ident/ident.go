// Package ident names the things a home is made of.
package ident

// ID names a Device, rule or plugin within a namespace. The host's store keeps saved state under
// its String form, so changing that form loses the state.
type ID struct {
	Namespace string
	Name      string
}

// String returns the ID as "<namespace>/<name>".
func (id ID) String() string {
	return id.Namespace + "/" + id.Name
}
