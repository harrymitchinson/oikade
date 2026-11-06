// Package wire is the protocol between the host and your process: newline-delimited JSON messages
// on a Unix socket. docs/host-protocol.md describes it.
package wire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/engine"
)

const (
	// Version is the protocol's version. A process sends it first, and the host refuses any other.
	Version = 1
	// RoleAutomations is the one process that runs a home's holds and moments.
	RoleAutomations = "automations"
	// SocketEnv names the host's socket to the process it starts.
	SocketEnv = "OIKADE_SOCKET"
	// Namespace is the home's namespace in the engine, whose rules and devices are keyed by one.
	Namespace = "home"

	// maxMessage bounds one message. A welcome lists every device, so it's generous.
	maxMessage = 16 << 20
	// writeTimeout bounds one write, so a process that stopped reading can't hold the writer.
	writeTimeout = 5 * time.Second
)

// Type is a message's type.
type Type string

// Message types. Hello and Welcome, or Rejected, open a session, and Ping, Pong, Change and
// Discovered flow for as long as it lasts. The rest are requests, each answered by a Reply with
// its ID.
const (
	TypeHello      Type = "hello"
	TypeWelcome    Type = "welcome"
	TypeRejected   Type = "rejected"
	TypePing       Type = "ping"
	TypePong       Type = "pong"
	TypeChange     Type = "change"
	TypeDiscovered Type = "discovered"
	TypeClaim      Type = "claim"
	TypeRelease    Type = "release"
	TypeReleaseAll Type = "releaseAll"
	TypeSet        Type = "set"
	TypeSaveHold   Type = "saveHold"
	TypeDeleteHold Type = "deleteHold"
	TypeSaveFire   Type = "saveFire"
	TypeJournal    Type = "journal"
	TypeFinish     Type = "finish"
	TypeReply      Type = "reply"
)

// Device is one device as the host knows it.
type Device struct {
	Name string `json:"name"`
	Room string `json:"room,omitempty"`
	// Arbitrated are the capabilities the host arbitrates, which take claims rather than writes.
	Arbitrated   []string `json:"arbitrated,omitempty"`
	Discovered   bool     `json:"discovered,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Values are bools and numbers, by capability.
	Values map[string]any `json:"values,omitempty"`
}

// Run is a flow's run that hasn't finished: its journal so far, oldest first.
type Run struct {
	ID      string   `json:"id"`
	Entries [][]byte `json:"entries"`
}

// Message is one line on the socket. Only the fields its Type uses are set.
type Message struct {
	Type Type `json:"type"`
	// ID matches a request to its Reply, and a Ping to its Pong.
	ID uint64 `json:"id,omitempty"`

	// Version and Role are a Hello's.
	Version int    `json:"version,omitempty"`
	Role    string `json:"role,omitempty"`
	// Rules names the process's holds, so the Welcome can return their saved state.
	Rules []string `json:"rules,omitempty"`

	// Devices, Holds, Runs and Fires are a Welcome's, and Error a Rejected's or a failed Reply's.
	Devices []Device                    `json:"devices,omitempty"`
	Holds   map[string]engine.HoldState `json:"holds,omitempty"`
	Runs    []Run                       `json:"runs,omitempty"`
	Fires   map[string]time.Time        `json:"fires,omitempty"`
	Error   string                      `json:"error,omitempty"`

	// Device, Capability, Value and Previous address a device.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	Previous   any    `json:"previous,omitempty"`
	// Discovered is a Discovered's device.
	Discovered *Device   `json:"discovered,omitempty"`
	Time       time.Time `json:"time,omitzero"`

	// Claim is a claim's, and Source a release's.
	Claim  *control.Claim `json:"claim,omitempty"`
	Source string         `json:"source,omitempty"`
	// Rule and Hold are a saved hold's. Rule and Time are a timer's last accounted fire.
	Rule string            `json:"rule,omitempty"`
	Hold *engine.HoldState `json:"hold,omitempty"`
	// Run and Data are a journal entry's, and Seq its place in the run.
	Run  string `json:"run,omitempty"`
	Data []byte `json:"data,omitempty"`
	Seq  uint64 `json:"seq,omitempty"`

	// Arbitrated is a claim or release's Reply: false means the capability isn't.
	Arbitrated bool `json:"arbitrated,omitempty"`
}

// Conn reads and writes Messages. One goroutine may Read while others Write.
type Conn struct {
	conn net.Conn
	read *bufio.Scanner
	mu   sync.Mutex
}

// NewConn wraps a socket.
func NewConn(c net.Conn) *Conn {
	r := bufio.NewScanner(c)
	r.Buffer(make([]byte, 0, 64<<10), maxMessage)
	return &Conn{conn: c, read: r}
}

// Read returns the next message.
func (c *Conn) Read() (Message, error) {
	if !c.read.Scan() {
		if err := c.read.Err(); err != nil {
			return Message{}, err
		}
		return Message{}, net.ErrClosed
	}
	var m Message
	if err := json.Unmarshal(c.read.Bytes(), &m); err != nil {
		return Message{}, fmt.Errorf("decoding message: %w", err)
	}
	return m, nil
}

// Write sends a message.
func (c *Conn) Write(m Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", m.Type, err)
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
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close closes the socket, which ends a Read in progress.
func (c *Conn) Close() error { return c.conn.Close() }

// ErrRejected is a session the host refused.
var ErrRejected = errors.New("host refused the session")
