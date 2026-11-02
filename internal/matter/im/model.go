// Package im is Matter's Interaction Model (Matter Core Specification, chapter 8) and the data
// model it serves (chapter 7): a Node of endpoints, each with clusters of attributes and
// commands, read, written, invoked and subscribed to over secure sessions.
package im

import (
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// Global attribute IDs, which every cluster has (Core spec, 7.13).
const (
	AttrGeneratedCommandList uint32 = 0xFFF8
	AttrAcceptedCommandList  uint32 = 0xFFF9
	AttrAttributeList        uint32 = 0xFFFB
	AttrFeatureMap           uint32 = 0xFFFC
	AttrClusterRevision      uint32 = 0xFFFD
)

// The Descriptor cluster's PartsList, which lists the endpoints under an endpoint.
const (
	descriptorCluster uint32 = 0x001D
	attrPartsList     uint32 = 3
)

var globalAttributes = []uint32{AttrGeneratedCommandList, AttrAcceptedCommandList, AttrAttributeList, AttrFeatureMap, AttrClusterRevision}

// WriteOp says how a write changes a list attribute.
type WriteOp uint8

const (
	// Replace sets the whole value.
	Replace WriteOp = iota
	// Append adds one item to a list, for a list written in chunks.
	Append
)

// Attribute is one attribute of a cluster.
type Attribute struct {
	ID uint32
	// Read writes the value with tag. It runs under the Node's read lock.
	Read func(req *Request, w *tlv.Writer, tag tlv.Tag) error
	// Write sets the value from r, which is on it. nil makes the attribute read-only. It runs
	// under the Node's lock, and calls Cluster.Changed if the value changed.
	Write func(req *Request, r *tlv.Reader, op WriteOp) Status
	// The privileges reading and writing need: View and Operate if zero.
	ReadPrivilege, WritePrivilege Privilege
	// FabricScoped attributes are lists whose entries each belong to a fabric. Only a subject
	// on a fabric may write them.
	FabricScoped bool
}

// Command is one command a cluster accepts.
type Command struct {
	ID uint32
	// Invoke runs the command. fields is on its fields structure, or nil if it had none. It runs
	// under the Node's lock. It returns nil, a Status or a ClusterStatus.
	Invoke func(req *Request, fields *tlv.Reader) (*Response, error)
	// Timed commands must come in a timed interaction.
	Timed bool
	// Privilege is what invoking the command needs: Operate if zero.
	Privilege Privilege
}

// Response is a command's response: another command, from server to client, with fields.
type Response struct {
	Command uint32
	// Fields writes the response's fields inside its structure.
	Fields func(w *tlv.Writer)
}

// Cluster is one cluster on an endpoint.
type Cluster struct {
	ID         uint32
	Revision   uint16
	FeatureMap uint32
	Attributes []Attribute
	Commands   []Command
	// Generated are the response commands the cluster sends.
	Generated []uint32

	version  uint32
	endpoint *Endpoint
}

// Node returns the Node the cluster was added to, or nil before it's added.
func (c *Cluster) Node() *Node {
	if c.endpoint == nil {
		return nil
	}
	return c.endpoint.node
}

// DataVersion returns the cluster's data version, which changes whenever an attribute does.
func (c *Cluster) DataVersion() uint32 { return c.version }

// Changed records that attribute attr changed. Call it with the Node's lock held, as Node.Update,
// Attribute.Write and Command.Invoke do. Subscribers hear of it when the lock is released.
func (c *Cluster) Changed(attr uint32) {
	c.version++
	if e := c.endpoint; e != nil && e.node != nil {
		e.node.pending = append(e.node.pending, Path{Endpoint: e.ID, Cluster: c.ID, Attribute: attr})
	}
}

func (c *Cluster) attribute(id uint32) *Attribute {
	i, ok := slices.BinarySearchFunc(c.Attributes, id, func(a Attribute, id uint32) int { return cmp.Compare(a.ID, id) })
	if !ok {
		return nil
	}
	return &c.Attributes[i]
}

func (c *Cluster) command(id uint32) *Command {
	for i := range c.Commands {
		if c.Commands[i].ID == id {
			return &c.Commands[i]
		}
	}
	return nil
}

// attributeIDs lists the cluster's attributes, global ones included, in order.
func (c *Cluster) attributeIDs() []uint32 {
	ids := make([]uint32, 0, len(c.Attributes)+len(globalAttributes))
	for _, a := range c.Attributes {
		ids = append(ids, a.ID)
	}
	return append(ids, globalAttributes...)
}

// readGlobal writes a global attribute.
func (c *Cluster) readGlobal(id uint32, w *tlv.Writer, tag tlv.Tag) bool {
	list := func(ids []uint32) {
		w.StartArray(tag)
		for _, id := range ids {
			w.PutUint(tlv.Anonymous, uint64(id))
		}
		w.End()
	}
	switch id {
	case AttrGeneratedCommandList:
		list(c.Generated)
	case AttrAcceptedCommandList:
		ids := make([]uint32, len(c.Commands))
		for i, cmd := range c.Commands {
			ids[i] = cmd.ID
		}
		list(ids)
	case AttrAttributeList:
		list(c.attributeIDs())
	case AttrFeatureMap:
		w.PutUint(tag, uint64(c.FeatureMap))
	case AttrClusterRevision:
		w.PutUint(tag, uint64(c.Revision))
	default:
		return false
	}
	return true
}

// DeviceType is a device type an endpoint implements.
type DeviceType struct {
	ID       uint32
	Revision uint16
}

// Endpoint is one endpoint of a Node.
type Endpoint struct {
	ID          uint16
	DeviceTypes []DeviceType
	Clusters    []*Cluster // in ID order once added
	// Parent is the endpoint this one is part of, such as a bridge's aggregator. nil for the
	// root endpoint and its direct parts.
	Parent *Endpoint

	node *Node
}

// Cluster returns the endpoint's cluster id, or nil.
func (e *Endpoint) Cluster(id uint32) *Cluster {
	for _, c := range e.Clusters {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// Path is a concrete attribute path.
type Path struct {
	Endpoint  uint16
	Cluster   uint32
	Attribute uint32
}

// Node is the data model a server serves. Its lock guards every cluster's state.
type Node struct {
	mu        sync.RWMutex
	endpoints []*Endpoint // in ID order
	pending   []Path      // changes made under the lock, announced when it's released
	listeners map[int]func([]Path)
	nextID    int
	listenMu  sync.Mutex
}

// AddEndpoint adds e and its clusters to the Node. Subscribers hear of every attribute of e, and
// of the PartsList of the root endpoint and of each of e's ancestors, in one change.
func (n *Node) AddEndpoint(e *Endpoint) error {
	var err error
	n.Update(func() {
		i, found := slices.BinarySearchFunc(n.endpoints, e.ID, func(x *Endpoint, id uint16) int { return cmp.Compare(x.ID, id) })
		if found {
			err = fmt.Errorf("matter: endpoint %d already exists", e.ID)
			return
		}
		e.node = n
		slices.SortFunc(e.Clusters, func(a, b *Cluster) int { return cmp.Compare(a.ID, b.ID) })
		for _, c := range e.Clusters {
			c.endpoint = e
			slices.SortFunc(c.Attributes, func(a, b Attribute) int { return cmp.Compare(a.ID, b.ID) })
			c.version = randomVersion()
			for _, id := range c.attributeIDs() {
				n.pending = append(n.pending, Path{e.ID, c.ID, id})
			}
		}
		n.endpoints = slices.Insert(n.endpoints, i, e)
		for _, x := range n.endpoints {
			if x == e || (x.ID != 0 && !isAncestor(x, e)) {
				continue
			}
			if d := x.Cluster(descriptorCluster); d != nil {
				d.Changed(attrPartsList)
			}
		}
	})
	return err
}

// RemoveEndpoint removes endpoint id and the endpoints under it. Subscribers hear of the PartsList
// of the root endpoint and of each of its ancestors. It reports whether the endpoint was there.
func (n *Node) RemoveEndpoint(id uint16) bool {
	found := false
	n.Update(func() {
		e := n.Endpoint(id)
		if e == nil || id == 0 {
			return
		}
		found = true
		for _, x := range n.endpoints {
			if x != e && !isAncestor(e, x) && (x.ID == 0 || isAncestor(x, e)) {
				if d := x.Cluster(descriptorCluster); d != nil {
					d.Changed(attrPartsList)
				}
			}
		}
		n.endpoints = slices.DeleteFunc(n.endpoints, func(x *Endpoint) bool { return x == e || isAncestor(e, x) })
		n.pending = slices.DeleteFunc(n.pending, func(p Path) bool { return n.Endpoint(p.Endpoint) == nil })
	})
	return found
}

func isAncestor(a, e *Endpoint) bool {
	for p := e.Parent; p != nil; p = p.Parent {
		if p == a {
			return true
		}
	}
	return false
}

// Endpoint returns endpoint id, or nil. Call it with the lock held.
func (n *Node) Endpoint(id uint16) *Endpoint {
	i, ok := slices.BinarySearchFunc(n.endpoints, id, func(x *Endpoint, id uint16) int { return cmp.Compare(x.ID, id) })
	if !ok {
		return nil
	}
	return n.endpoints[i]
}

// Endpoints returns the endpoints in ID order. Call it with the lock held.
func (n *Node) Endpoints() []*Endpoint { return n.endpoints }

// Parts returns the endpoints that are part of e: every other endpoint for the root endpoint 0,
// and e's descendants otherwise. Call it with the lock held.
func (n *Node) Parts(e *Endpoint) []uint16 {
	var ids []uint16
	for _, x := range n.endpoints {
		if x == e {
			continue
		}
		if e.ID == 0 {
			ids = append(ids, x.ID)
			continue
		}
		for p := x.Parent; p != nil; p = p.Parent {
			if p == e {
				ids = append(ids, x.ID)
				break
			}
		}
	}
	return ids
}

// Update runs f with the Node's lock held, for changes from outside the Interaction Model, such
// as a device reporting new state. f calls Cluster.Changed for what it changes.
func (n *Node) Update(f func()) {
	n.mu.Lock()
	f()
	changes := n.pending
	n.pending = nil
	n.mu.Unlock()
	n.announce(changes)
}

// View runs f with the Node's read lock held.
func (n *Node) View(f func()) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	f()
}

// listen calls f with every batch of changes until the returned function is called.
func (n *Node) listen(f func([]Path)) (stop func()) {
	n.listenMu.Lock()
	defer n.listenMu.Unlock()
	if n.listeners == nil {
		n.listeners = map[int]func([]Path){}
	}
	id := n.nextID
	n.nextID++
	n.listeners[id] = f
	return func() {
		n.listenMu.Lock()
		defer n.listenMu.Unlock()
		delete(n.listeners, id)
	}
}

func (n *Node) announce(changes []Path) {
	if len(changes) == 0 {
		return
	}
	n.listenMu.Lock()
	ls := make([]func([]Path), 0, len(n.listeners))
	for _, f := range n.listeners {
		ls = append(ls, f)
	}
	n.listenMu.Unlock()
	for _, f := range ls {
		f(changes)
	}
}

func randomVersion() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}
