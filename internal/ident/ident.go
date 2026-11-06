// Package ident names the things a home is made of.
package ident

// Separator is what String puts between an ID's namespace and name.
const Separator = "/"

// ID names a Device, rule or plugin within a namespace. The host's store keeps saved state under
// its String form, so changing that form loses the state.
type ID struct {
	Namespace string
	Name      string
}

// String returns the ID as "<namespace>/<name>".
func (id ID) String() string {
	return id.Namespace + Separator + id.Name
}
