// Package pluginwire is the part of the host's protocol that a plugin process speaks. It's apart
// from package wire, which needs the engine's types, because the public SDK imports it, and a plugin
// shouldn't link the engine to say hello.
package pluginwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

const (
	// Version is the protocol's version. A process sends it first, and the host refuses any other.
	// Raise it with any change to what a message means: a host outlives a deploy, so it can meet a
	// binary built against something else. Version 2 added releaseAll's below, and 3 pausing rules,
	// the rule facts in a hello, and reporting rule runs and metrics, and 4
	// plugin sessions and a Manifest's plugins.
	Version = 5
	// RolePlugin is a plugin your home imports. The host starts one process for each.
	RolePlugin = "plugin"

	// maxMessage bounds one message. A welcome lists every device, so it's generous.
	maxMessage = 16 << 20
	// writeTimeout bounds one write, so a process that stopped reading can't hold the writer.
	writeTimeout = 5 * time.Second
)

// Type is a message's type.
type Type string

// A plugin's session has only these message types: Hello, Welcome or Rejected, then Ping, Pong and
// Publish, which flows both ways.
const (
	TypeHello    Type = "hello"
	TypeWelcome  Type = "welcome"
	TypeRejected Type = "rejected"
	TypePing     Type = "ping"
	TypePong     Type = "pong"
	TypePublish  Type = "publish"
)

// Message is one line of a plugin's session. Its fields are those of wire.Message, under the same
// names, because the host reads a hello before it knows who's calling.
type Message struct {
	Type Type `json:"type"`
	// ID matches a Ping to its Pong.
	ID uint64 `json:"id,omitempty"`
	// Version and Role are a Hello's.
	Version int    `json:"version,omitempty"`
	Role    string `json:"role,omitempty"`
	// Plugin is a Hello's: which of the binary's plugins this process is.
	Plugin string `json:"plugin,omitempty"`
	Error  string `json:"error,omitempty"`
	// Topic, Data and Retain are a Publish's. A Welcome's Topic is the plugin's root.
	Topic  string `json:"topic,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Retain bool   `json:"retain,omitempty"`
	// Declared and SetTimeout are a Welcome's: the devices on record for the plugin, and how long a
	// Set may run before the plugin counts as hung.
	Declared   []contractv1.DeclaredDevice `json:"declared,omitempty"`
	SetTimeout time.Duration               `json:"setTimeout,omitempty"`
}

// Conn reads and writes messages of type M. One goroutine may Read while others Write.
type Conn[M any] struct {
	conn net.Conn
	read *bufio.Scanner
	mu   sync.Mutex
}

// NewConn wraps a socket.
func NewConn[M any](c net.Conn) *Conn[M] {
	r := bufio.NewScanner(c)
	r.Buffer(make([]byte, 0, 64<<10), maxMessage)
	return &Conn[M]{conn: c, read: r}
}

// Read returns the next message.
func (c *Conn[M]) Read() (M, error) {
	var m M
	if !c.read.Scan() {
		if err := c.read.Err(); err != nil {
			return m, err
		}
		return m, net.ErrClosed
	}
	if err := json.Unmarshal(c.read.Bytes(), &m); err != nil {
		return m, fmt.Errorf("decoding message: %w", err)
	}
	return m, nil
}

// Write sends a message.
func (c *Conn[M]) Write(m M) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err = c.conn.Write(append(raw, '\n'))
	return err
}

// SetReadDeadline bounds the next Read. Zero means none.
func (c *Conn[M]) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close closes the socket, which ends a Read in progress.
func (c *Conn[M]) Close() error { return c.conn.Close() }

// ErrRejected is a session the host refused.
var ErrRejected = errors.New("host refused the session")

// Dial opens a plugin's session and returns its connection and Welcome.
func Dial(ctx context.Context, socket, name string) (*Conn[Message], Message, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, Message{}, fmt.Errorf("connecting to the host: %w", err)
	}
	conn := NewConn[Message](raw)
	if err := conn.Write(Message{Type: TypeHello, Version: Version, Role: RolePlugin, Plugin: name}); err != nil {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("greeting the host: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	welcome, err := conn.Read()
	if err != nil {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("reading the host's welcome: %w", err)
	}
	if welcome.Type != TypeWelcome {
		_ = conn.Close()
		return nil, Message{}, fmt.Errorf("%w: %s", ErrRejected, welcome.Error)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, welcome, nil
}
