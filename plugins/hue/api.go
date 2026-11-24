package hue

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"oikade.dev/internal/mdns"
)

const (
	// maxRead is the most a read of every resource may be: a bridge with 66 devices answers 380 KB.
	maxRead = 32 << 20
	// maxEvent is the most one event in the stream may be.
	maxEvent = 4 << 20
	// maxError is the most of a refusal that's read, for what it says.
	maxError = 4 << 10
)

// client speaks the bridge's local API, CLIP v2, with the key setup got from it.
type client struct {
	base string
	key  string
	// reads bounds each request, and stream doesn't, as the event stream lasts as long as it can.
	reads, stream *http.Client
}

// dialBridge dials the bridge, finding a .local name over mDNS. Tests replace it.
var dialBridge = mdns.DialContext

func newClient(address, key string, config *tls.Config, t timing) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = config
	transport.DialContext = dialBridge
	transport.ForceAttemptHTTP2 = true
	// The bridge sends nothing while nothing changes, so a ping is how a connection that's gone shows.
	transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: t.ping, PingTimeout: t.pong}
	return &client{
		base:   "https://" + address,
		key:    key,
		reads:  &http.Client{Transport: transport, Timeout: t.request},
		stream: &http.Client{Transport: transport},
	}
}

// close drops the client's connections.
func (c *client) close() { c.reads.CloseIdleConnections() }

// apiError is an answer the bridge refused a request with.
type apiError struct {
	status      int
	description string
}

func (e *apiError) Error() string {
	if e.description == "" {
		return fmt.Sprintf("the bridge answered %d %s", e.status, http.StatusText(e.status))
	}
	return fmt.Sprintf("the bridge answered %d %s: %s", e.status, http.StatusText(e.status), e.description)
}

// refusedKey is whether err is the bridge refusing the key, as it does one removed in the Hue app.
func refusedKey(err error) bool {
	e, ok := errors.AsType[*apiError](err)
	return ok && (e.status == http.StatusUnauthorized || e.status == http.StatusForbidden)
}

// limitedRequests is whether err is the bridge saying there were too many requests.
func limitedRequests(err error) bool {
	e, ok := errors.AsType[*apiError](err)
	return ok && e.status == http.StatusTooManyRequests
}

// answer is the envelope of every CLIP v2 answer.
type answer struct {
	Errors []struct {
		Description string `json:"description"`
	} `json:"errors"`
	Data []json.RawMessage `json:"data"`
}

// resources reads every resource the bridge has.
func (c *client) resources(ctx context.Context) ([]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/clip/v2/resource", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(c.reads, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRead+1))
	if err != nil {
		return nil, fmt.Errorf("could not read the bridge's resources: %w", err)
	}
	if len(raw) > maxRead {
		return nil, errors.New("the bridge's resources are too long to read")
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("could not read the bridge's resources: %w", err)
	}
	if len(a.Errors) > 0 {
		return nil, &apiError{status: resp.StatusCode, description: a.Errors[0].Description}
	}
	return a.Data, nil
}

// events opens the bridge's event stream.
func (c *client) events(ctx context.Context) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/eventstream/clip/v2", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.do(c.stream, req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// do sends a request with the key, and turns an answer other than 200 into an apiError.
func (c *client) do(h *http.Client, req *http.Request) (*http.Response, error) {
	req.Header.Set("hue-application-key", c.key)
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxError))
	e := &apiError{status: resp.StatusCode}
	var a answer
	if json.Unmarshal(raw, &a) == nil && len(a.Errors) > 0 {
		e.description = a.Errors[0].Description
	}
	return nil, e
}

// eventReader reads the event stream, which is server-sent events: each event's data is a list of
// the bridge's events, as JSON.
type eventReader struct {
	r *bufio.Reader
}

func newEventReader(r io.Reader) *eventReader { return &eventReader{r: bufio.NewReaderSize(r, 64<<10)} }

// errEventTooLong is an event longer than maxEvent, which can't be read.
var errEventTooLong = errors.New("an event in the stream is too long to read")

// next returns the next event's data, skipping comments and the fields it doesn't need. An event's
// data lines are joined with newlines, as the format says.
func (e *eventReader) next() ([]byte, error) {
	var data []byte
	has := false
	for {
		line, err := e.line()
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			if has {
				return data, nil
			}
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if string(field) != "data" {
			continue // a comment, such as the stream's ": hi", an id or a field it doesn't use
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if has {
			data = append(data, '\n')
		}
		if len(data)+len(value) > maxEvent {
			return nil, errEventTooLong
		}
		data = append(data, value...)
		has = true
	}
}

// line reads one line, without its line ending, LF or CRLF.
func (e *eventReader) line() ([]byte, error) {
	var line []byte
	for {
		chunk, err := e.r.ReadSlice('\n')
		if len(line)+len(chunk) > maxEvent+2 {
			return nil, errEventTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// update is one of the bridge's events: a list of resources, each with only what changed.
type update struct {
	Type string            `json:"type"`
	Data []json.RawMessage `json:"data"`
}

// parseEvents reads an event's data, a list of the bridge's events.
func parseEvents(data []byte) ([]update, error) {
	var updates []update
	if err := json.Unmarshal(data, &updates); err != nil {
		return nil, fmt.Errorf("could not read an event: %w", err)
	}
	return updates, nil
}
