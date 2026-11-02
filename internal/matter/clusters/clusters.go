// Package clusters implements the Matter clusters a bridge serves (Matter Application Cluster
// Specification), and lays them out as a bridge: the root endpoint, an aggregator, and one
// bridged endpoint per device.
package clusters

import (
	"errors"
	"slices"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// Cluster IDs.
const (
	IDOnOff                         uint32 = 0x0006
	IDDescriptor                    uint32 = 0x001D
	IDBasicInformation              uint32 = 0x0028
	IDBridgedDeviceBasicInformation uint32 = 0x0039
)

// Device types (Matter Device Library Specification).
var (
	DeviceRootNode    = im.DeviceType{ID: 0x0016, Revision: 2}
	DeviceAggregator  = im.DeviceType{ID: 0x000E, Revision: 1}
	DeviceBridgedNode = im.DeviceType{ID: 0x0013, Revision: 2}
	DeviceOnOffLight  = im.DeviceType{ID: 0x0100, Revision: 3}
	DeviceOnOffPlug   = im.DeviceType{ID: 0x010A, Revision: 3}
)

// What the bridge says of the Matter version it implements (Basic Information).
const (
	dataModelRevision    = 17         // Matter 1.3
	specificationVersion = 0x01030000 // 1.3.0
	maxStringLength      = 32
)

// readOnly returns an attribute whose value write puts.
func readOnly(id uint32, write func(w *tlv.Writer, tag tlv.Tag)) im.Attribute {
	return im.Attribute{ID: id, Read: func(_ *im.Request, w *tlv.Writer, tag tlv.Tag) error { write(w, tag); return nil }}
}

func uintAttr(id uint32, v uint64) im.Attribute {
	return readOnly(id, func(w *tlv.Writer, tag tlv.Tag) { w.PutUint(tag, v) })
}

func stringAttr(id uint32, v *string) im.Attribute {
	return readOnly(id, func(w *tlv.Writer, tag tlv.Tag) { w.PutUTF8(tag, *v) })
}

// writableString is a string attribute a client with priv may set, of minLen to limit bytes.
func writableString(c **im.Cluster, id uint32, v *string, minLen, limit int, priv im.Privilege) im.Attribute {
	a := stringAttr(id, v)
	a.WritePrivilege = priv
	a.Write = func(_ *im.Request, r *tlv.Reader, op im.WriteOp) im.Status {
		if op != im.Replace {
			return im.StatusInvalidAction
		}
		s, err := r.UTF8()
		var te *tlv.ErrType
		switch {
		case errors.As(err, &te):
			return im.StatusInvalidDataType
		case err != nil:
			return im.StatusConstraintError
		case len(s) < minLen || len(s) > limit:
			return im.StatusConstraintError
		}
		if s != *v {
			*v = s
			(*c).Changed(id)
		}
		return im.StatusSuccess
	}
	return a
}

// Descriptor describes an endpoint: its device types, clusters and parts.
func Descriptor(node *im.Node, e *im.Endpoint) *im.Cluster {
	return &im.Cluster{
		ID: IDDescriptor, Revision: 2,
		Attributes: []im.Attribute{
			readOnly(0, func(w *tlv.Writer, tag tlv.Tag) { // DeviceTypeList
				w.StartArray(tag)
				for _, d := range e.DeviceTypes {
					w.StartStructure(tlv.Anonymous)
					w.PutUint(tlv.Context(0), uint64(d.ID))
					w.PutUint(tlv.Context(1), uint64(d.Revision))
					w.End()
				}
				w.End()
			}),
			readOnly(1, func(w *tlv.Writer, tag tlv.Tag) { // ServerList
				w.StartArray(tag)
				for _, c := range e.Clusters {
					w.PutUint(tlv.Anonymous, uint64(c.ID))
				}
				w.End()
			}),
			readOnly(2, func(w *tlv.Writer, tag tlv.Tag) { // ClientList
				w.StartArray(tag)
				w.End()
			}),
			readOnly(3, func(w *tlv.Writer, tag tlv.Tag) { // PartsList
				w.StartArray(tag)
				for _, id := range node.Parts(e) {
					w.PutUint(tlv.Anonymous, uint64(id))
				}
				w.End()
			}),
		},
	}
}

// Info is what the bridge says of itself in Basic Information.
type Info struct {
	VendorName            string
	VendorID              uint16
	ProductName           string
	ProductID             uint16
	NodeLabel             string
	HardwareVersion       uint16
	HardwareVersionString string
	SoftwareVersion       uint32
	SoftwareVersionString string
	UniqueID              string
}

// BasicInformation is the root endpoint's Basic Information cluster. NodeLabel and Location
// are writable.
func BasicInformation(info Info) *im.Cluster {
	var c *im.Cluster
	location := "XX"
	c = &im.Cluster{
		ID: IDBasicInformation, Revision: 3,
		Attributes: []im.Attribute{
			uintAttr(0x00, dataModelRevision),
			stringAttr(0x01, &info.VendorName),
			uintAttr(0x02, uint64(info.VendorID)),
			stringAttr(0x03, &info.ProductName),
			uintAttr(0x04, uint64(info.ProductID)),
			writableString(&c, 0x05, &info.NodeLabel, 0, maxStringLength, im.PrivilegeManage),
			writableString(&c, 0x06, &location, 2, 2, im.PrivilegeAdminister), // an ISO 3166-1 alpha-2 code
			uintAttr(0x07, uint64(info.HardwareVersion)),
			stringAttr(0x08, &info.HardwareVersionString),
			uintAttr(0x09, uint64(info.SoftwareVersion)),
			stringAttr(0x0A, &info.SoftwareVersionString),
			stringAttr(0x12, &info.UniqueID),
			readOnly(0x13, func(w *tlv.Writer, tag tlv.Tag) { // CapabilityMinima
				w.StartStructure(tag)
				w.PutUint(tlv.Context(0), 3) // CASE sessions per fabric
				w.PutUint(tlv.Context(1), 3) // subscriptions per fabric
				w.End()
			}),
			uintAttr(0x15, specificationVersion),
			uintAttr(0x16, 1), // MaxPathsPerInvoke
		},
	}
	return c
}

// BridgedInfo is a bridged device's Bridged Device Basic Information.
type BridgedInfo struct {
	cluster   *im.Cluster
	label     string
	uniqueID  string
	reachable bool
}

// NewBridgedInfo returns the cluster for one bridged device.
func NewBridgedInfo(label, uniqueID string) *BridgedInfo {
	b := &BridgedInfo{label: label, uniqueID: uniqueID, reachable: true}
	b.cluster = &im.Cluster{
		ID: IDBridgedDeviceBasicInformation, Revision: 3,
		Attributes: []im.Attribute{
			stringAttr(0x05, &b.label),
			readOnly(0x11, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, b.reachable) }),
			stringAttr(0x12, &b.uniqueID),
		},
	}
	return b
}

// Cluster returns the cluster to add to the device's endpoint.
func (b *BridgedInfo) Cluster() *im.Cluster { return b.cluster }

// SetReachable records whether the device answers. Call it inside Node.Update.
func (b *BridgedInfo) SetReachable(r bool) {
	if r != b.reachable {
		b.reachable = r
		b.cluster.Changed(0x11)
	}
}

// OnOff is an On/Off cluster (without the Lighting feature).
type OnOff struct {
	cluster *im.Cluster
	on      bool
	// OnChange is called, under the Node's lock, when a client turns it on or off. It must not
	// block.
	OnChange func(on bool)
}

// NewOnOff returns an On/Off cluster.
func NewOnOff() *OnOff {
	o := &OnOff{}
	set := func(on bool) func(*im.Request, *tlv.Reader) (*im.Response, error) {
		return func(*im.Request, *tlv.Reader) (*im.Response, error) {
			o.command(on)
			return nil, nil
		}
	}
	o.cluster = &im.Cluster{
		ID: IDOnOff, Revision: 6,
		Attributes: []im.Attribute{
			readOnly(0x0000, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, o.on) }),
		},
		Commands: []im.Command{
			{ID: 0x00, Invoke: set(false)}, // Off
			{ID: 0x01, Invoke: set(true)},  // On
			{ID: 0x02, Invoke: func(*im.Request, *tlv.Reader) (*im.Response, error) { // Toggle
				o.command(!o.on)
				return nil, nil
			}},
		},
	}
	return o
}

// Cluster returns the cluster to add to an endpoint.
func (o *OnOff) Cluster() *im.Cluster { return o.cluster }

// On reports the state. Call it inside Node.View or Node.Update.
func (o *OnOff) On() bool { return o.on }

// Set records the device's state, as reported by the device. Call it inside Node.Update.
func (o *OnOff) Set(on bool) {
	if on != o.on {
		o.on = on
		o.cluster.Changed(0x0000)
	}
}

func (o *OnOff) command(on bool) {
	o.Set(on)
	if o.OnChange != nil {
		o.OnChange(on)
	}
}

// Bridge is a Node laid out as a Matter bridge: endpoint 0 is the root node, endpoint 1 the
// aggregator, and each bridged device has an endpoint under the aggregator.
type Bridge struct {
	Node       *im.Node
	Root       *im.Endpoint
	Aggregator *im.Endpoint
}

// NewBridge returns a bridge with no devices. rootClusters join the Descriptor, Basic Information
// and General Diagnostics on the root endpoint, such as the commissioning clusters.
func NewBridge(info Info, rootClusters ...*im.Cluster) *Bridge {
	b := &Bridge{Node: &im.Node{}}
	b.Root = &im.Endpoint{ID: 0, DeviceTypes: []im.DeviceType{DeviceRootNode}}
	b.Root.Clusters = append([]*im.Cluster{Descriptor(b.Node, b.Root), BasicInformation(info), GeneralDiagnostics(time.Now())}, rootClusters...)
	b.Aggregator = &im.Endpoint{ID: 1, DeviceTypes: []im.DeviceType{DeviceAggregator}}
	b.Aggregator.Clusters = []*im.Cluster{Descriptor(b.Node, b.Aggregator)}
	_ = b.Node.AddEndpoint(b.Root) // a new Node: the IDs are free
	_ = b.Node.AddEndpoint(b.Aggregator)
	return b
}

// AddDevice adds a bridged device on endpoint id, with Identify, its own clusters, Descriptor and
// Bridged Device Basic Information, and returns its BridgedInfo. It fails if the endpoint is
// taken.
func (b *Bridge) AddDevice(id uint16, deviceType im.DeviceType, label, uniqueID string, clusters ...*im.Cluster) (*BridgedInfo, error) {
	return b.AddDeviceTypes(id, []im.DeviceType{deviceType}, label, uniqueID, clusters...)
}

// AddDeviceTypes is AddDevice for a device that is several device types on one endpoint, such as
// an occupancy sensor that also measures temperature and runs on a battery.
func (b *Bridge) AddDeviceTypes(id uint16, deviceTypes []im.DeviceType, label, uniqueID string, clusters ...*im.Cluster) (*BridgedInfo, error) {
	info := NewBridgedInfo(label, uniqueID)
	e := &im.Endpoint{ID: id, Parent: b.Aggregator, DeviceTypes: append(slices.Clone(deviceTypes), DeviceBridgedNode)}
	e.Clusters = append(append([]*im.Cluster{newIdentify()}, clusters...), Descriptor(b.Node, e), info.Cluster())
	if err := b.Node.AddEndpoint(e); err != nil {
		return nil, err
	}
	return info, nil
}

// RemoveDevice removes the bridged device on endpoint id, and reports whether there was one.
func (b *Bridge) RemoveDevice(id uint16) bool {
	if id <= b.Aggregator.ID {
		return false
	}
	return b.Node.RemoveEndpoint(id)
}
