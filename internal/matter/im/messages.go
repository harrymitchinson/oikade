package im

import (
	"errors"
	"fmt"
	"io"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Interaction Model opcodes (Core spec, 10.2.1).
const (
	OpStatusResponse    uint8 = 0x01
	OpReadRequest       uint8 = 0x02
	OpSubscribeRequest  uint8 = 0x03
	OpSubscribeResponse uint8 = 0x04
	OpReportData        uint8 = 0x05
	OpWriteRequest      uint8 = 0x06
	OpWriteResponse     uint8 = 0x07
	OpInvokeRequest     uint8 = 0x08
	OpInvokeResponse    uint8 = 0x09
	OpTimedRequest      uint8 = 0x0A
)

// revision is the Interaction Model revision every message carries, at tag 0xFF.
const revision = 11

var tagRevision = tlv.Context(0xFF)

// Status is an Interaction Model status code (Core spec, 8.10).
type Status uint8

// Status codes.
const (
	StatusSuccess               Status = 0x00
	StatusFailure               Status = 0x01
	StatusInvalidSubscription   Status = 0x7D
	StatusUnsupportedAccess     Status = 0x7E
	StatusUnsupportedEndpoint   Status = 0x7F
	StatusInvalidAction         Status = 0x80
	StatusUnsupportedCommand    Status = 0x81
	StatusInvalidCommand        Status = 0x85
	StatusUnsupportedAttribute  Status = 0x86
	StatusConstraintError       Status = 0x87
	StatusUnsupportedWrite      Status = 0x88
	StatusResourceExhausted     Status = 0x89
	StatusNotFound              Status = 0x8B
	StatusInvalidDataType       Status = 0x8D
	StatusDataVersionMismatch   Status = 0x92
	StatusTimeout               Status = 0x94
	StatusBusy                  Status = 0x9C
	StatusUnsupportedCluster    Status = 0xC3
	StatusNeedsTimedInteraction Status = 0xC6
	StatusTimedRequestMismatch  Status = 0xC9
	StatusFailsafeRequired      Status = 0xCA
	StatusInvalidInState        Status = 0xCB
)

// statusNames are the Status codes' names, as the Core spec gives them in snake case.
var statusNames = map[Status]string{
	StatusSuccess: "success", StatusFailure: "failure", StatusInvalidSubscription: "invalid_subscription",
	StatusUnsupportedAccess: "unsupported_access", StatusUnsupportedEndpoint: "unsupported_endpoint",
	StatusInvalidAction: "invalid_action", StatusUnsupportedCommand: "unsupported_command",
	StatusInvalidCommand: "invalid_command", StatusUnsupportedAttribute: "unsupported_attribute",
	StatusConstraintError: "constraint_error", StatusUnsupportedWrite: "unsupported_write",
	StatusResourceExhausted: "resource_exhausted", StatusNotFound: "not_found",
	StatusInvalidDataType: "invalid_data_type", StatusDataVersionMismatch: "data_version_mismatch",
	StatusTimeout: "timeout", StatusBusy: "busy", StatusUnsupportedCluster: "unsupported_cluster",
	StatusNeedsTimedInteraction: "needs_timed_interaction", StatusTimedRequestMismatch: "timed_request_mismatch",
	StatusFailsafeRequired: "failsafe_required", StatusInvalidInState: "invalid_in_state",
}

// Name is the status's name in snake case, such as unsupported_access, or its code for one with no name.
func (s Status) Name() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return fmt.Sprintf("%#02x", uint8(s))
}

func (s Status) Error() string {
	return fmt.Sprintf("matter: interaction model status %#02x", uint8(s))
}

// errInvalid wraps every error from decoding an Interaction Model message.
var errInvalid = errors.New("matter: invalid interaction model message")

// AttributePath is an attribute path that may have wildcards (Core spec, 10.6.2).
type AttributePath struct {
	Endpoint      uint16
	Cluster       uint32
	Attribute     uint32
	WildEndpoint  bool
	WildCluster   bool
	WildAttribute bool
	// Append is a null ListIndex: the data is one item to add to a list.
	Append bool
}

// Concrete returns the path for a concrete attribute.
func Concrete(p Path) AttributePath {
	return AttributePath{Endpoint: p.Endpoint, Cluster: p.Cluster, Attribute: p.Attribute}
}

func (p AttributePath) wild() bool { return p.WildEndpoint || p.WildCluster || p.WildAttribute }

func (p AttributePath) matches(c Path) bool {
	return (p.WildEndpoint || p.Endpoint == c.Endpoint) && (p.WildCluster || p.Cluster == c.Cluster) &&
		(p.WildAttribute || p.Attribute == c.Attribute)
}

func (p AttributePath) put(w *tlv.Writer, tag tlv.Tag) {
	w.StartList(tag)
	if !p.WildEndpoint {
		w.PutUint(tlv.Context(2), uint64(p.Endpoint))
	}
	if !p.WildCluster {
		w.PutUint(tlv.Context(3), uint64(p.Cluster))
	}
	if !p.WildAttribute {
		w.PutUint(tlv.Context(4), uint64(p.Attribute))
	}
	if p.Append {
		w.PutNull(tlv.Context(5))
	}
	w.End()
}

func readAttributePath(r *tlv.Reader) (AttributePath, error) {
	p := AttributePath{WildEndpoint: true, WildCluster: true, WildAttribute: true}
	err := fields(r, func(r *tlv.Reader, tag uint8) error {
		switch tag {
		case 2:
			v, err := uintField(r, 0xFFFF)
			p.Endpoint, p.WildEndpoint = uint16(v), false
			return err
		case 3:
			v, err := uintField(r, 0xFFFFFFFF)
			p.Cluster, p.WildCluster = uint32(v), false
			return err
		case 4:
			v, err := uintField(r, 0xFFFFFFFF)
			p.Attribute, p.WildAttribute = uint32(v), false
			return err
		case 5:
			if !r.IsNull() {
				return fmt.Errorf("%w: list index other than null", errInvalid)
			}
			p.Append = true
		}
		return nil // 0 (tag compression) and 1 (node) don't change what we serve
	})
	return p, err
}

// CommandPath is a concrete command path.
type CommandPath struct {
	Endpoint uint16
	Cluster  uint32
	Command  uint32
}

func (p CommandPath) put(w *tlv.Writer, tag tlv.Tag) {
	w.StartList(tag)
	w.PutUint(tlv.Context(0), uint64(p.Endpoint))
	w.PutUint(tlv.Context(1), uint64(p.Cluster))
	w.PutUint(tlv.Context(2), uint64(p.Command))
	w.End()
}

func readCommandPath(r *tlv.Reader) (CommandPath, error) {
	var p CommandPath
	var seen [3]bool
	err := fields(r, func(r *tlv.Reader, tag uint8) error {
		var err error
		var v uint64
		switch tag {
		case 0:
			v, err = uintField(r, 0xFFFF)
			p.Endpoint = uint16(v)
		case 1:
			v, err = uintField(r, 0xFFFFFFFF)
			p.Cluster = uint32(v)
		case 2:
			v, err = uintField(r, 0xFFFFFFFF)
			p.Command = uint32(v)
		default:
			return nil
		}
		seen[tag] = true
		return err
	})
	if err == nil && (!seen[0] || !seen[1] || !seen[2]) {
		err = fmt.Errorf("%w: a command path needs its endpoint, cluster and command", errInvalid)
	}
	return p, err
}

type dataVersionFilter struct {
	endpoint uint16
	cluster  uint32
	version  uint32
}

type readRequest struct {
	attributes []AttributePath
	filters    []dataVersionFilter
	// subscribe only
	fabricFiltered    bool
	keepSubscriptions bool
	minFloor          uint16
	maxCeiling        uint16
}

// decodeReadRequest decodes a Read Request, or a Subscribe Request if subscribe.
func decodeReadRequest(b []byte, subscribe bool) (*readRequest, error) {
	m := &readRequest{}
	tagAttrs, tagFilters, tagFabricFiltered := uint8(0), uint8(4), uint8(3)
	if subscribe {
		tagAttrs, tagFilters, tagFabricFiltered = 3, 8, 7
	}
	err := message(b, func(r *tlv.Reader, tag uint8) error {
		switch {
		case tag == tagAttrs:
			return items(r, func(r *tlv.Reader) error {
				p, err := readAttributePath(r)
				m.attributes = append(m.attributes, p)
				return err
			})
		case tag == tagFilters:
			return items(r, func(r *tlv.Reader) error {
				f, err := readDataVersionFilter(r)
				m.filters = append(m.filters, f)
				return err
			})
		case tag == tagFabricFiltered:
			v, err := r.Bool()
			m.fabricFiltered = v
			return err
		case subscribe && tag == 0:
			v, err := r.Bool()
			m.keepSubscriptions = v
			return err
		case subscribe && tag == 1:
			v, err := uintField(r, 0xFFFF)
			m.minFloor = uint16(v)
			return err
		case subscribe && tag == 2:
			v, err := uintField(r, 0xFFFF)
			m.maxCeiling = uint16(v)
			return err
		}
		return nil // events and event filters: we have no events
	})
	return m, err
}

// readDataVersionFilter reads a DataVersionFilterIB: a cluster path and the version the client
// already has.
func readDataVersionFilter(r *tlv.Reader) (dataVersionFilter, error) {
	var f dataVersionFilter
	var seen [4]bool
	err := fields(r, func(r *tlv.Reader, tag uint8) error {
		switch tag {
		case 0:
			seen[0] = true
			return fields(r, func(r *tlv.Reader, tag uint8) error {
				switch tag {
				case 1:
					v, err := uintField(r, 0xFFFF)
					f.endpoint, seen[1] = uint16(v), true
					return err
				case 2:
					v, err := uintField(r, 0xFFFFFFFF)
					f.cluster, seen[2] = uint32(v), true
					return err
				}
				return nil
			})
		case 1:
			v, err := uintField(r, 0xFFFFFFFF)
			f.version, seen[3] = uint32(v), true
			return err
		}
		return nil
	})
	if err == nil && seen != [4]bool{true, true, true, true} {
		err = fmt.Errorf("%w: data version filter needs an endpoint, a cluster and a version", errInvalid)
	}
	return f, err
}

type writeRequest struct {
	suppressResponse bool
	timed            bool
	writes           []attributeData
	more             bool
}

type attributeData struct {
	path       AttributePath
	hasVersion bool
	version    uint32
	data       []byte // the encoded data element
}

func decodeWriteRequest(b []byte) (*writeRequest, error) {
	m := &writeRequest{}
	err := message(b, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			m.suppressResponse, err = r.Bool()
		case 1:
			m.timed, err = r.Bool()
		case 2:
			return items(r, func(r *tlv.Reader) error {
				d, err := readAttributeData(r)
				m.writes = append(m.writes, d)
				return err
			})
		case 3:
			m.more, err = r.Bool()
		}
		return err
	})
	return m, err
}

func readAttributeData(r *tlv.Reader) (attributeData, error) {
	var d attributeData
	var hasPath bool
	err := fields(r, func(r *tlv.Reader, tag uint8) error {
		switch tag {
		case 0:
			v, err := uintField(r, 0xFFFFFFFF)
			d.hasVersion, d.version = true, uint32(v)
			return err
		case 1:
			hasPath = true
			var err error
			d.path, err = readAttributePath(r)
			return err
		case 2:
			var err error
			d.data, err = element(r)
			return err
		}
		return nil
	})
	if err == nil && (!hasPath || d.data == nil) {
		err = fmt.Errorf("%w: attribute data needs a path and data", errInvalid)
	}
	return d, err
}

type invokeRequest struct {
	suppressResponse bool
	timed            bool
	commands         []commandData
}

type commandData struct {
	path   CommandPath
	fields []byte // the encoded fields structure, or nil
	ref    *uint16
}

func decodeInvokeRequest(b []byte) (*invokeRequest, error) {
	m := &invokeRequest{}
	err := message(b, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			m.suppressResponse, err = r.Bool()
		case 1:
			m.timed, err = r.Bool()
		case 2:
			return items(r, func(r *tlv.Reader) error {
				var c commandData
				var hasPath bool
				err := fields(r, func(r *tlv.Reader, tag uint8) (err error) {
					switch tag {
					case 0:
						hasPath = true
						c.path, err = readCommandPath(r)
					case 1:
						c.fields, err = element(r)
					case 2:
						var v uint64
						v, err = uintField(r, 0xFFFF)
						ref := uint16(v)
						c.ref = &ref
					}
					return err
				})
				if err == nil && !hasPath {
					err = fmt.Errorf("%w: command without a path", errInvalid)
				}
				m.commands = append(m.commands, c)
				return err
			})
		}
		return err
	})
	return m, err
}

func decodeTimedRequest(b []byte) (uint16, error) {
	var timeout uint16
	var seen bool
	err := message(b, func(r *tlv.Reader, tag uint8) error {
		if tag != 0 {
			return nil
		}
		v, err := uintField(r, 0xFFFF)
		timeout, seen = uint16(v), true
		return err
	})
	if err == nil && !seen {
		err = fmt.Errorf("%w: timed request without a timeout", errInvalid)
	}
	return timeout, err
}

func decodeStatusResponse(b []byte) (Status, error) {
	var s Status
	var seen bool
	err := message(b, func(r *tlv.Reader, tag uint8) error {
		if tag != 0 {
			return nil
		}
		v, err := uintField(r, 0xFF)
		s, seen = Status(v), true
		return err
	})
	if err == nil && !seen {
		err = fmt.Errorf("%w: status response without a status", errInvalid)
	}
	return s, err
}

func statusResponse(s Status) transport.Message {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutUint(tlv.Context(0), uint64(s))
	w.PutUint(tagRevision, revision)
	w.End()
	return imMessage(OpStatusResponse, w.Bytes())
}

func putStatus(w *tlv.Writer, tag tlv.Tag, s Status) {
	w.StartStructure(tag)
	w.PutUint(tlv.Context(0), uint64(s))
	w.End()
}

func imMessage(op uint8, payload []byte) transport.Message {
	return transport.Message{Protocol: transport.InteractionModel, Opcode: op, Payload: payload}
}

// message reads an IM message: an anonymous structure, calling field for each context tag.
// Nothing may follow it.
func message(b []byte, field func(r *tlv.Reader, tag uint8) error) error {
	r := tlv.NewReader(b)
	if err := r.Next(); err != nil {
		return fmt.Errorf("%w: %w", errInvalid, err)
	}
	if err := fields(r, field); err != nil {
		return err
	}
	if err := r.Next(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: bytes after the message", errInvalid)
	}
	return nil
}

// fields enters the structure or list r is on and calls field for each context-tagged element,
// failing if a tag repeats. It leaves the container.
func fields(r *tlv.Reader, field func(r *tlv.Reader, tag uint8) error) error {
	if t := r.Type(); t != tlv.Structure && t != tlv.List {
		return fmt.Errorf("%w: %s where a structure was expected", errInvalid, t)
	}
	if err := r.Enter(); err != nil {
		return fmt.Errorf("%w: %w", errInvalid, err)
	}
	var seen [256]bool
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", errInvalid, err)
		}
		tag := r.Tag()
		if tag.Kind != tlv.ContextTag {
			continue
		}
		if seen[tag.Number] {
			return fmt.Errorf("%w: field %d repeated", errInvalid, tag.Number)
		}
		seen[tag.Number] = true
		if err := field(r, uint8(tag.Number)); err != nil {
			if !errors.Is(err, errInvalid) {
				err = fmt.Errorf("%w: field %d: %w", errInvalid, tag.Number, err)
			}
			return err
		}
	}
	if err := r.Exit(); err != nil {
		return fmt.Errorf("%w: %w", errInvalid, err)
	}
	return nil
}

// items enters the array or list r is on and calls item for each element.
func items(r *tlv.Reader, item func(r *tlv.Reader) error) error {
	if t := r.Type(); t != tlv.Array && t != tlv.List {
		return fmt.Errorf("%w: %s where an array was expected", errInvalid, t)
	}
	if err := r.Enter(); err != nil {
		return fmt.Errorf("%w: %w", errInvalid, err)
	}
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", errInvalid, err)
		}
		if err := item(r); err != nil {
			return err
		}
	}
	if err := r.Exit(); err != nil {
		return fmt.Errorf("%w: %w", errInvalid, err)
	}
	return nil
}

func uintField(r *tlv.Reader, limit uint64) (uint64, error) {
	v, err := r.Uint()
	if err != nil {
		return 0, err
	}
	if v > limit {
		return 0, fmt.Errorf("%w: %d is out of range", errInvalid, v)
	}
	return v, nil
}

// element returns the encoded bytes of the element r is on, re-encoded with an anonymous tag,
// so it can be read again on its own.
func element(r *tlv.Reader) ([]byte, error) {
	var w tlv.Writer
	if err := copyElement(r, &w, tlv.Anonymous); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

// copyElement writes the element r is on, and everything in it, to w with tag.
func copyElement(r *tlv.Reader, w *tlv.Writer, tag tlv.Tag) error {
	switch r.Type() {
	case tlv.Int:
		v, err := r.Int()
		w.PutInt(tag, v)
		return err
	case tlv.Uint:
		v, err := r.Uint()
		w.PutUint(tag, v)
		return err
	case tlv.Bool:
		v, err := r.Bool()
		w.PutBool(tag, v)
		return err
	case tlv.Float:
		if r.Size() == 4 {
			v, err := r.Float32()
			w.PutFloat32(tag, v)
			return err
		}
		v, err := r.Float()
		w.PutFloat64(tag, v)
		return err
	case tlv.UTF8:
		v, err := r.UTF8()
		w.PutUTF8(tag, v)
		return err
	case tlv.Bytes:
		v, err := r.Bytes()
		w.PutBytes(tag, v)
		return err
	case tlv.Null:
		w.PutNull(tag)
		return nil
	}
	switch r.Type() {
	case tlv.Structure:
		w.StartStructure(tag)
	case tlv.Array:
		w.StartArray(tag)
	case tlv.List:
		w.StartList(tag)
	}
	if err := r.Enter(); err != nil {
		return err
	}
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := copyElement(r, w, r.Tag()); err != nil {
			return err
		}
	}
	w.End()
	return r.Exit()
}

// Fields enters the structure r is on, such as a command's fields, and calls field for each
// context-tagged element. It fails if a tag repeats, and leaves the structure.
func Fields(r *tlv.Reader, field func(r *tlv.Reader, tag uint8) error) error { return fields(r, field) }

// Items enters the array or list r is on and calls item for each element.
func Items(r *tlv.Reader, item func(r *tlv.Reader) error) error { return items(r, item) }
