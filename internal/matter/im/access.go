package im

import (
	"fmt"

	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Privilege is an access privilege (Core spec, 6.6.2), numbered as the Access Control cluster's
// AccessControlEntryPrivilegeEnum.
type Privilege uint8

// Privileges. Each one but ProxyView grants those below it, and Administer grants ProxyView.
const (
	PrivilegeView       Privilege = 1
	PrivilegeProxyView  Privilege = 2
	PrivilegeOperate    Privilege = 3
	PrivilegeManage     Privilege = 4
	PrivilegeAdminister Privilege = 5
)

// Grants reports whether holding p gives want.
func (p Privilege) Grants(want Privilege) bool {
	switch {
	case p == want:
		return true
	case p == PrivilegeAdminister:
		return true
	case want == PrivilegeProxyView:
		return false // only Administer and ProxyView itself
	case p == PrivilegeProxyView:
		return want == PrivilegeView
	}
	return p > want && want != 0
}

// Subject is who a request comes from: its session's authentication (Core spec, 6.6.2.1).
type Subject struct {
	Session     *transport.Session
	AuthMode    transport.AuthMode
	FabricIndex uint8  // the accessing fabric, 0 for none
	NodeID      uint64 // the peer's operational node ID, for CASE
	CATs        []uint32
}

// Request is the context an attribute's Read and Write, and a command's Invoke, run in.
type Request struct {
	Subject
	// FabricFiltered asks a read of a fabric-scoped list for the accessing fabric's entries only.
	FabricFiltered bool
}

// newRequest describes a request on session.
func newRequest(session *transport.Session, fabricFiltered bool) *Request {
	return &Request{
		Session:        session,
		AuthMode:       session.AuthMode(),
		FabricIndex:    session.FabricIndex(),
		NodeID:         session.PeerNodeID(),
		CATs:           session.CATs(),
		FabricFiltered: fabricFiltered,
	}
}

// AccessControl decides what a CASE subject may do, from the Access Control cluster's entries.
type AccessControl interface {
	// Allowed reports whether s holds priv on cluster of endpoint e. It runs under the Node's
	// lock.
	Allowed(s *Subject, e *Endpoint, cluster uint32, priv Privilege) bool
}

// allowed checks a subject's access. A PASE session holds Administer, as it does while it
// commissions the node (Core spec, 6.6.2.9). Every other subject needs an entry that grants it.
func (s *Server) allowed(req *Request, e *Endpoint, cluster uint32, priv Privilege) bool {
	switch req.AuthMode {
	case transport.AuthPASE:
		return true
	case transport.AuthCASE:
		return s.Access != nil && s.Access.Allowed(&req.Subject, e, cluster, priv)
	}
	return false
}

func (a *Attribute) readPrivilege() Privilege {
	if a == nil || a.ReadPrivilege == 0 {
		return PrivilegeView
	}
	return a.ReadPrivilege
}

func (a *Attribute) writePrivilege() Privilege {
	if a.WritePrivilege == 0 {
		return PrivilegeOperate
	}
	return a.WritePrivilege
}

func (c *Command) privilege() Privilege {
	if c == nil || c.Privilege == 0 {
		return PrivilegeOperate
	}
	return c.Privilege
}

// ClusterStatus is a command's failure with a cluster-specific status code.
type ClusterStatus struct {
	Status Status // usually StatusFailure
	Code   uint8
}

func (c ClusterStatus) Error() string {
	return fmt.Sprintf("matter: interaction model status %#02x, cluster status %#02x", uint8(c.Status), c.Code)
}
