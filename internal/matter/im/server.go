package im

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

const (
	// reportBudget is how many bytes of attribute reports go in one Report Data message, leaving
	// room in transport.MaxMessageSize for the headers, the integrity check and the message's own
	// fields.
	reportBudget = 1024
	// responseTimeout bounds how long the server waits for a client's Status Response.
	responseTimeout = 30 * time.Second
	// maxSubscriptions caps the subscriptions the server holds.
	maxSubscriptions = 32
)

// Server serves a Node's Interaction Model on secure sessions. Access control isn't checked yet:
// every secure session may read, write and invoke everything.
type Server struct {
	Node *Node
	Log  logr.Logger

	mu   sync.Mutex
	subs map[uint32]*subscription
}

// Register starts serving on tr.
func (s *Server) Register(tr *transport.Transport) {
	tr.Handle(transport.InteractionModel,
		[]uint8{OpReadRequest, OpSubscribeRequest, OpWriteRequest, OpInvokeRequest, OpTimedRequest}, s.serve)
}

func (s *Server) serve(ctx context.Context, ex *transport.Exchange, m transport.Message) {
	defer ex.Close()
	if !ex.Session().Secure() {
		s.Log.Info("Refused interaction over an unsecured session", "peer", ex.Session().Peer())
		return
	}
	var err error
	switch m.Opcode {
	case OpReadRequest:
		err = s.read(ctx, ex, m.Payload)
	case OpSubscribeRequest:
		err = s.subscribe(ctx, ex, m.Payload)
	case OpWriteRequest:
		err = s.write(ctx, ex, m.Payload, false)
	case OpInvokeRequest:
		err = s.invoke(ctx, ex, m.Payload, false)
	case OpTimedRequest:
		err = s.timed(ctx, ex, m.Payload)
	}
	if err != nil {
		s.Log.V(1).Info("Interaction failed", "peer", ex.Session().Peer(), "opcode", m.Opcode, "error", err.Error())
	}
}

// fail answers a whole request with a status.
func fail(ctx context.Context, ex *transport.Exchange, st Status, err error) error {
	_ = ex.Send(ctx, statusResponse(st))
	return err
}

func (s *Server) read(ctx context.Context, ex *transport.Exchange, b []byte) error {
	req, err := decodeReadRequest(b, false)
	if err != nil {
		return fail(ctx, ex, StatusInvalidAction, err)
	}
	var reports [][]byte
	s.Node.View(func() { reports = s.reports(req.attributes, req.filters) })
	return sendReports(ctx, ex, reports, nil, true)
}

// reports builds an AttributeReportIB for each attribute the paths name. A concrete path that
// names nothing gets a status; wildcards skip what's missing. Call it with the Node's read lock.
func (s *Server) reports(paths []AttributePath, filters []dataVersionFilter) [][]byte {
	var out [][]byte
	seen := map[Path]bool{}
	for _, p := range paths {
		if !p.wild() {
			if st := s.check(p); st != StatusSuccess {
				out = append(out, statusReport(p, st))
				continue
			}
		}
		for _, e := range s.Node.endpoints {
			if !p.WildEndpoint && e.ID != p.Endpoint {
				continue
			}
			for _, c := range e.Clusters {
				if !p.WildCluster && c.ID != p.Cluster {
					continue
				}
				if p.wild() && filtered(filters, e.ID, c) {
					continue
				}
				for _, id := range c.attributeIDs() {
					if !p.WildAttribute && id != p.Attribute {
						continue
					}
					path := Path{e.ID, c.ID, id}
					if seen[path] {
						continue
					}
					seen[path] = true
					out = append(out, dataReport(c, path))
				}
			}
		}
	}
	return out
}

// check says whether a concrete path names an attribute.
func (s *Server) check(p AttributePath) Status {
	e := s.Node.Endpoint(p.Endpoint)
	if e == nil {
		return StatusUnsupportedEndpoint
	}
	c := e.Cluster(p.Cluster)
	if c == nil {
		return StatusUnsupportedCluster
	}
	if c.attribute(p.Attribute) == nil && !isGlobal(p.Attribute) {
		return StatusUnsupportedAttribute
	}
	return StatusSuccess
}

func isGlobal(id uint32) bool {
	return slices.Contains(globalAttributes, id)
}

// filtered reports whether the client already has this version of the cluster.
func filtered(filters []dataVersionFilter, ep uint16, c *Cluster) bool {
	for _, f := range filters {
		if f.endpoint == ep && f.cluster == c.ID && f.version == c.version {
			return true
		}
	}
	return false
}

// dataReport encodes an AttributeReportIB with the attribute's value, or a failure status if it
// can't be read or is too large for a report.
func dataReport(c *Cluster, p Path) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.StartStructure(tlv.Context(1)) // AttributeDataIB
	w.PutUint(tlv.Context(0), uint64(c.version))
	Concrete(p).put(&w, tlv.Context(1))
	ok := c.readGlobal(p.Attribute, &w, tlv.Context(2))
	if !ok {
		a := c.attribute(p.Attribute)
		if a == nil || a.Read == nil || a.Read(&w, tlv.Context(2)) != nil {
			return statusReport(Concrete(p), StatusFailure)
		}
	}
	w.End()
	w.End()
	if w.Len() > reportBudget {
		return statusReport(Concrete(p), StatusResourceExhausted)
	}
	return w.Bytes()
}

// statusReport encodes an AttributeReportIB with a status.
func statusReport(p AttributePath, st Status) []byte {
	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.StartStructure(tlv.Context(0)) // AttributeStatusIB
	p.put(&w, tlv.Context(0))
	putStatus(&w, tlv.Context(1), st)
	w.End()
	w.End()
	return w.Bytes()
}

// sendReports sends reports in as many Report Data messages as they need, waiting for the
// client's Status Response after each one but a final suppressed one. subID is set for a
// subscription.
func sendReports(ctx context.Context, ex *transport.Exchange, reports [][]byte, subID *uint32, suppressLast bool) error {
	chunks := [][][]byte{nil}
	size := 0
	for _, r := range reports {
		if size > 0 && size+len(r) > reportBudget {
			chunks = append(chunks, nil)
			size = 0
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], r)
		size += len(r)
	}
	for i, chunk := range chunks {
		more := i < len(chunks)-1
		suppress := suppressLast && !more
		var w tlv.Writer
		w.StartStructure(tlv.Anonymous)
		if subID != nil {
			w.PutUint(tlv.Context(0), uint64(*subID))
		}
		if len(chunk) > 0 {
			w.StartArray(tlv.Context(1))
			for _, r := range chunk {
				w.PutRaw(r)
			}
			w.End()
		}
		if more {
			w.PutBool(tlv.Context(3), true)
		}
		if suppress {
			w.PutBool(tlv.Context(4), true)
		}
		w.PutUint(tagRevision, revision)
		w.End()
		if err := ex.Send(ctx, imMessage(OpReportData, w.Bytes())); err != nil {
			return err
		}
		if suppress {
			return nil
		}
		if err := expectStatus(ctx, ex); err != nil {
			return err
		}
	}
	return nil
}

// expectStatus waits for a successful Status Response.
func expectStatus(ctx context.Context, ex *transport.Exchange) error {
	ctx, cancel := context.WithTimeout(ctx, responseTimeout)
	defer cancel()
	m, err := ex.Recv(ctx)
	if err != nil {
		return err
	}
	if m.Protocol != transport.InteractionModel || m.Opcode != OpStatusResponse {
		return fmt.Errorf("%w: opcode %#x instead of a status response", errInvalid, m.Opcode)
	}
	st, err := decodeStatusResponse(m.Payload)
	if err != nil {
		return err
	}
	if st != StatusSuccess {
		return st
	}
	return nil
}

func (s *Server) write(ctx context.Context, ex *transport.Exchange, b []byte, timed bool) error {
	for {
		req, err := decodeWriteRequest(b)
		if err != nil {
			return fail(ctx, ex, StatusInvalidAction, err)
		}
		if req.timed != timed {
			return fail(ctx, ex, StatusTimedRequestMismatch, errors.New("timed flag doesn't match the interaction"))
		}
		statuses := make([]Status, len(req.writes))
		s.Node.Update(func() {
			for i, d := range req.writes {
				statuses[i] = s.apply(d)
			}
		})
		if !req.suppressResponse {
			var w tlv.Writer
			w.StartStructure(tlv.Anonymous)
			w.StartArray(tlv.Context(0))
			for i, d := range req.writes {
				w.StartStructure(tlv.Anonymous) // AttributeStatusIB
				p := d.path
				p.Append = false
				p.put(&w, tlv.Context(0))
				putStatus(&w, tlv.Context(1), statuses[i])
				w.End()
			}
			w.End()
			w.PutUint(tagRevision, revision)
			w.End()
			if err := ex.Send(ctx, imMessage(OpWriteResponse, w.Bytes())); err != nil {
				return err
			}
		}
		if !req.more {
			return nil
		}
		// A chunked write: the next chunk comes on the same exchange.
		next, err := recvWithin(ctx, ex)
		if err != nil {
			return err
		}
		if next.Opcode != OpWriteRequest {
			return fail(ctx, ex, StatusInvalidAction, fmt.Errorf("opcode %#x in a chunked write", next.Opcode))
		}
		b = next.Payload
	}
}

// apply performs one attribute write. Call it with the Node's lock.
func (s *Server) apply(d attributeData) Status {
	if d.path.wild() {
		return StatusInvalidAction // writes name one attribute
	}
	if st := s.check(d.path); st != StatusSuccess {
		return st
	}
	c := s.Node.Endpoint(d.path.Endpoint).Cluster(d.path.Cluster)
	a := c.attribute(d.path.Attribute)
	if a == nil || a.Write == nil {
		return StatusUnsupportedWrite
	}
	if d.hasVersion && d.version != c.version {
		return StatusDataVersionMismatch
	}
	r := tlv.NewReader(d.data)
	if err := r.Next(); err != nil {
		return StatusInvalidDataType
	}
	op := Replace
	if d.path.Append {
		op = Append
	}
	return a.Write(r, op)
}

func (s *Server) invoke(ctx context.Context, ex *transport.Exchange, b []byte, timed bool) error {
	req, err := decodeInvokeRequest(b)
	if err != nil {
		return fail(ctx, ex, StatusInvalidAction, err)
	}
	if req.timed != timed {
		return fail(ctx, ex, StatusTimedRequestMismatch, errors.New("timed flag doesn't match the interaction"))
	}
	if len(req.commands) != 1 {
		// MaxPathsPerInvoke is 1.
		return fail(ctx, ex, StatusInvalidAction, fmt.Errorf("%d commands in one invoke", len(req.commands)))
	}
	cmd := req.commands[0]
	var resp *Response
	var st Status
	s.Node.Update(func() { resp, st = s.run(cmd, timed) })
	if req.suppressResponse {
		return nil
	}

	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutBool(tlv.Context(0), false)
	w.StartArray(tlv.Context(1))
	w.StartStructure(tlv.Anonymous) // InvokeResponseIB
	if resp != nil && st == StatusSuccess {
		w.StartStructure(tlv.Context(0)) // CommandDataIB
		CommandPath{cmd.path.Endpoint, cmd.path.Cluster, resp.Command}.put(&w, tlv.Context(0))
		w.StartStructure(tlv.Context(1))
		if resp.Fields != nil {
			resp.Fields(&w)
		}
		w.End()
	} else {
		w.StartStructure(tlv.Context(1)) // CommandStatusIB
		cmd.path.put(&w, tlv.Context(0))
		putStatus(&w, tlv.Context(1), st)
	}
	if cmd.ref != nil {
		w.PutUint(tlv.Context(2), uint64(*cmd.ref))
	}
	w.End()
	w.End()
	w.End()
	w.PutUint(tagRevision, revision)
	w.End()
	return ex.Send(ctx, imMessage(OpInvokeResponse, w.Bytes()))
}

// run invokes one command. Call it with the Node's lock.
func (s *Server) run(cmd commandData, timed bool) (*Response, Status) {
	e := s.Node.Endpoint(cmd.path.Endpoint)
	if e == nil {
		return nil, StatusUnsupportedEndpoint
	}
	c := e.Cluster(cmd.path.Cluster)
	if c == nil {
		return nil, StatusUnsupportedCluster
	}
	command := c.command(cmd.path.Command)
	if command == nil {
		return nil, StatusUnsupportedCommand
	}
	if command.Timed && !timed {
		return nil, StatusNeedsTimedInteraction
	}
	var fields *tlv.Reader
	if cmd.fields != nil {
		fields = tlv.NewReader(cmd.fields)
		if err := fields.Next(); err != nil || fields.Type() != tlv.Structure {
			return nil, StatusInvalidCommand
		}
	}
	return command.Invoke(fields)
}

// timed handles a Timed Request: the Write or Invoke that follows on the exchange must arrive
// within its timeout.
func (s *Server) timed(ctx context.Context, ex *transport.Exchange, b []byte) error {
	timeout, err := decodeTimedRequest(b)
	if err != nil {
		return fail(ctx, ex, StatusInvalidAction, err)
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	if err := ex.Send(ctx, statusResponse(StatusSuccess)); err != nil {
		return err
	}
	m, err := recvWithin(ctx, ex)
	if err != nil {
		return err
	}
	if time.Now().After(deadline) {
		return fail(ctx, ex, StatusTimeout, errors.New("timed request expired"))
	}
	switch m.Opcode {
	case OpWriteRequest:
		return s.write(ctx, ex, m.Payload, true)
	case OpInvokeRequest:
		return s.invoke(ctx, ex, m.Payload, true)
	}
	return fail(ctx, ex, StatusInvalidAction, fmt.Errorf("opcode %#x after a timed request", m.Opcode))
}

func recvWithin(ctx context.Context, ex *transport.Exchange) (transport.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, responseTimeout)
	defer cancel()
	m, err := ex.Recv(ctx)
	if err == nil && m.Protocol != transport.InteractionModel {
		err = fmt.Errorf("%w: protocol %#x", errInvalid, m.Protocol.ID)
	}
	return m, err
}

func randomSubscriptionID() uint32 {
	var b [4]byte
	for {
		_, _ = rand.Read(b[:])
		if id := binary.LittleEndian.Uint32(b[:]); id != 0 {
			return id
		}
	}
}
