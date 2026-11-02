package im

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// Client is a minimal Interaction Model client: enough to drive a server in tests and scripted
// checks, not a full controller.
type Client struct {
	Session *transport.Session

	mu   sync.Mutex
	subs map[uint32]chan []AttributeReport
}

// AttributeReport is one attribute's value or status from a report.
type AttributeReport struct {
	Path    AttributePath
	Version uint32
	Data    []byte // the encoded value, readable with DecodeValue; nil with a status
	Status  Status
}

// Read reads the attributes the paths name.
func (c *Client) Read(ctx context.Context, paths ...AttributePath) ([]AttributeReport, error) {
	ex, err := c.Session.NewExchange()
	if err != nil {
		return nil, err
	}
	defer ex.Close()
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	putPaths(&w, tlv.Context(0), paths)
	w.PutBool(tlv.Context(3), true)
	w.PutUint(tagRevision, revision)
	w.End()
	if err := ex.Send(ctx, imMessage(OpReadRequest, w.Bytes())); err != nil {
		return nil, err
	}
	reports, _, err := receiveReports(ctx, ex)
	return reports, err
}

// Write writes one attribute, with value writing it under the given tag. It returns the
// attribute's status.
func (c *Client) Write(ctx context.Context, path AttributePath, value func(w *tlv.Writer, tag tlv.Tag)) (Status, error) {
	ex, err := c.Session.NewExchange()
	if err != nil {
		return 0, err
	}
	defer ex.Close()
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.StartArray(tlv.Context(2))
	w.StartStructure(tlv.Anonymous)
	path.put(&w, tlv.Context(1))
	value(&w, tlv.Context(2))
	w.End()
	w.End()
	w.PutUint(tagRevision, revision)
	w.End()
	if err := ex.Send(ctx, imMessage(OpWriteRequest, w.Bytes())); err != nil {
		return 0, err
	}
	m, err := recvIM(ctx, ex)
	if err != nil {
		return 0, err
	}
	if m.Opcode == OpStatusResponse {
		st, err := decodeStatusResponse(m.Payload)
		if err == nil {
			err = st
		}
		return 0, err
	}
	var st Status
	err = message(m.Payload, func(r *tlv.Reader, tag uint8) error {
		if tag != 0 {
			return nil
		}
		return items(r, func(r *tlv.Reader) error {
			return fields(r, func(r *tlv.Reader, tag uint8) error {
				if tag == 1 {
					return readStatusIB(r, &st)
				}
				return nil
			})
		})
	})
	return st, err
}

// InvokeResult is a command's response: a status, or a response command and its fields.
type InvokeResult struct {
	Status   Status
	Response *CommandPath
	Fields   []byte // the encoded fields structure
}

// Invoke invokes a command, with fields writing its fields inside their structure (nil for
// none). If timed, it first sends a Timed Request with timeoutMs.
func (c *Client) Invoke(ctx context.Context, path CommandPath, fieldsFn func(w *tlv.Writer), timed bool, timeoutMs uint16) (*InvokeResult, error) {
	ex, err := c.Session.NewExchange()
	if err != nil {
		return nil, err
	}
	defer ex.Close()
	if timed {
		var w tlv.Writer
		w.StartStructure(tlv.Anonymous)
		w.PutUint(tlv.Context(0), uint64(timeoutMs))
		w.PutUint(tagRevision, revision)
		w.End()
		if err := ex.Send(ctx, imMessage(OpTimedRequest, w.Bytes())); err != nil {
			return nil, err
		}
		if err := expectStatus(ctx, ex); err != nil {
			return nil, err
		}
	}
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBool(tlv.Context(0), false)
	w.PutBool(tlv.Context(1), timed)
	w.StartArray(tlv.Context(2))
	w.StartStructure(tlv.Anonymous)
	path.put(&w, tlv.Context(0))
	w.StartStructure(tlv.Context(1))
	if fieldsFn != nil {
		fieldsFn(&w)
	}
	w.End()
	w.End()
	w.End()
	w.PutUint(tagRevision, revision)
	w.End()
	if err := ex.Send(ctx, imMessage(OpInvokeRequest, w.Bytes())); err != nil {
		return nil, err
	}
	m, err := recvIM(ctx, ex)
	if err != nil {
		return nil, err
	}
	if m.Opcode == OpStatusResponse {
		st, err := decodeStatusResponse(m.Payload)
		if err == nil {
			err = st
		}
		return nil, err
	}
	res := &InvokeResult{}
	err = message(m.Payload, func(r *tlv.Reader, tag uint8) error {
		if tag != 1 {
			return nil
		}
		return items(r, func(r *tlv.Reader) error {
			return fields(r, func(r *tlv.Reader, tag uint8) error {
				switch tag {
				case 0: // CommandDataIB
					return fields(r, func(r *tlv.Reader, tag uint8) (err error) {
						switch tag {
						case 0:
							var p CommandPath
							p, err = readCommandPath(r)
							res.Response = &p
						case 1:
							res.Fields, err = element(r)
						}
						return err
					})
				case 1: // CommandStatusIB
					return fields(r, func(r *tlv.Reader, tag uint8) error {
						if tag == 1 {
							return readStatusIB(r, &res.Status)
						}
						return nil
					})
				}
				return nil
			})
		})
	})
	return res, err
}

// Subscribe subscribes to the paths, and returns the priming report, the subscription's ID, its
// max interval in seconds and a channel of later reports. Register must have been called on the
// client's Transport.
func (c *Client) Subscribe(ctx context.Context, minFloor, maxCeiling uint16, paths ...AttributePath) ([]AttributeReport, uint32, uint16, <-chan []AttributeReport, error) {
	ex, err := c.Session.NewExchange()
	if err != nil {
		return nil, 0, 0, nil, err
	}
	defer ex.Close()
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBool(tlv.Context(0), false)
	w.PutUint(tlv.Context(1), uint64(minFloor))
	w.PutUint(tlv.Context(2), uint64(maxCeiling))
	putPaths(&w, tlv.Context(3), paths)
	w.PutBool(tlv.Context(7), true)
	w.PutUint(tagRevision, revision)
	w.End()
	if err := ex.Send(ctx, imMessage(OpSubscribeRequest, w.Bytes())); err != nil {
		return nil, 0, 0, nil, err
	}
	priming, id, err := receiveReports(ctx, ex)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	m, err := recvIM(ctx, ex)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	if m.Opcode != OpSubscribeResponse {
		return nil, 0, 0, nil, fmt.Errorf("%w: opcode %#x instead of a subscribe response", errInvalid, m.Opcode)
	}
	var gotID uint32
	var maxInterval uint16
	err = message(m.Payload, func(r *tlv.Reader, tag uint8) error {
		switch tag {
		case 0:
			v, err := uintField(r, 0xFFFFFFFF)
			gotID = uint32(v)
			return err
		case 2:
			v, err := uintField(r, 0xFFFF)
			maxInterval = uint16(v)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, 0, 0, nil, err
	}
	if gotID != id {
		return nil, 0, 0, nil, fmt.Errorf("%w: subscribe response for %d, reports for %d", errInvalid, gotID, id)
	}
	ch := make(chan []AttributeReport, 16)
	c.mu.Lock()
	if c.subs == nil {
		c.subs = map[uint32]chan []AttributeReport{}
	}
	c.subs[id] = ch
	c.mu.Unlock()
	return priming, id, maxInterval, ch, nil
}

// Register answers the reports that a server sends for this client's subscriptions on tr.
func (c *Client) Register(tr *transport.Transport) {
	tr.Handle(transport.InteractionModel, []uint8{OpReportData}, func(ctx context.Context, ex *transport.Exchange, first transport.Message) {
		defer ex.Close()
		reports, id, err := handleReports(ctx, ex, first)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch := c.subs[id]
		c.mu.Unlock()
		if ch == nil {
			return
		}
		select {
		case ch <- reports:
		default:
		}
	})
}

// receiveReports reads Report Data messages until the last chunk, answering each as needed.
func receiveReports(ctx context.Context, ex *transport.Exchange) ([]AttributeReport, uint32, error) {
	m, err := recvIM(ctx, ex)
	if err != nil {
		return nil, 0, err
	}
	return handleReports(ctx, ex, m)
}

func handleReports(ctx context.Context, ex *transport.Exchange, m transport.Message) ([]AttributeReport, uint32, error) {
	var all []AttributeReport
	for {
		if m.Opcode == OpStatusResponse {
			st, err := decodeStatusResponse(m.Payload)
			if err == nil {
				err = st
			}
			return nil, 0, err
		}
		if m.Opcode != OpReportData {
			return nil, 0, fmt.Errorf("%w: opcode %#x instead of a report", errInvalid, m.Opcode)
		}
		reports, id, more, suppress, err := decodeReportData(m.Payload)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, reports...)
		if !suppress {
			if err := ex.Send(ctx, statusResponse(StatusSuccess)); err != nil {
				return nil, 0, err
			}
		}
		if !more {
			return all, id, nil
		}
		if m, err = recvIM(ctx, ex); err != nil {
			return nil, 0, err
		}
	}
}

func decodeReportData(b []byte) (reports []AttributeReport, id uint32, more, suppress bool, err error) {
	err = message(b, func(r *tlv.Reader, tag uint8) (err error) {
		switch tag {
		case 0:
			var v uint64
			v, err = uintField(r, 0xFFFFFFFF)
			id = uint32(v)
		case 1:
			return items(r, func(r *tlv.Reader) error {
				var rep AttributeReport
				err := fields(r, func(r *tlv.Reader, tag uint8) error {
					switch tag {
					case 0: // AttributeStatusIB
						return fields(r, func(r *tlv.Reader, tag uint8) (err error) {
							switch tag {
							case 0:
								rep.Path, err = readAttributePath(r)
							case 1:
								err = readStatusIB(r, &rep.Status)
							}
							return err
						})
					case 1: // AttributeDataIB
						d, err := readAttributeData(r)
						rep.Path, rep.Version, rep.Data = d.path, d.version, d.data
						return err
					}
					return nil
				})
				reports = append(reports, rep)
				return err
			})
		case 3:
			more, err = r.Bool()
		case 4:
			suppress, err = r.Bool()
		}
		return err
	})
	return reports, id, more, suppress, err
}

func readStatusIB(r *tlv.Reader, st *Status) error {
	return fields(r, func(r *tlv.Reader, tag uint8) error {
		if tag == 0 {
			v, err := uintField(r, 0xFF)
			*st = Status(v)
			return err
		}
		return nil
	})
}

func putPaths(w *tlv.Writer, tag tlv.Tag, paths []AttributePath) {
	w.StartArray(tag)
	for _, p := range paths {
		p.put(w, tlv.Anonymous)
	}
	w.End()
}

func recvIM(ctx context.Context, ex *transport.Exchange) (transport.Message, error) {
	m, err := ex.Recv(ctx)
	if err == nil && m.Protocol != transport.InteractionModel {
		err = fmt.Errorf("%w: protocol %#x", errInvalid, m.Protocol.ID)
	}
	return m, err
}

// DecodeValue decodes an encoded value into Go values: int64, uint64, bool, float64, string,
// []byte, nil, []any for arrays and lists, and map[uint8]any for structures by context tag.
func DecodeValue(b []byte) (any, error) {
	r := tlv.NewReader(b)
	if err := r.Next(); err != nil {
		return nil, err
	}
	return decodeAny(r)
}

func decodeAny(r *tlv.Reader) (any, error) {
	switch r.Type() {
	case tlv.Int:
		return r.Int()
	case tlv.Uint:
		return r.Uint()
	case tlv.Bool:
		return r.Bool()
	case tlv.Float:
		return r.Float()
	case tlv.UTF8:
		return r.UTF8()
	case tlv.Bytes:
		b, err := r.Bytes()
		return append([]byte(nil), b...), err
	case tlv.Null:
		return nil, nil
	}
	isStruct := r.Type() == tlv.Structure
	if err := r.Enter(); err != nil {
		return nil, err
	}
	var list []any
	st := map[uint8]any{}
	for {
		err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		tag := r.Tag()
		v, err := decodeAny(r)
		if err != nil {
			return nil, err
		}
		if isStruct {
			st[uint8(tag.Number)] = v
		} else {
			list = append(list, v)
		}
	}
	if err := r.Exit(); err != nil {
		return nil, err
	}
	if isStruct {
		return st, nil
	}
	return list, nil
}
